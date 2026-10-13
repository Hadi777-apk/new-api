package model

import (
	"errors"
	"time"

	"gorm.io/gorm"
)

// Only metadata is stored in SQL. Private request/result files share the drawing
// volume; the idempotency tombstone outlives their 24-hour retention.
type APIImageTask struct {
	ID           string `json:"id" gorm:"type:varchar(36);primaryKey"`
	UserID       int    `json:"-" gorm:"uniqueIndex:api_image_submission,priority:1;index"`
	TokenID      int    `json:"-" gorm:"uniqueIndex:api_image_submission,priority:2"`
	SubmissionID string `json:"-" gorm:"type:varchar(36);uniqueIndex:api_image_submission,priority:3"`
	RequestHash  string `json:"request_hash" gorm:"type:varchar(64)"`
	ClientIP     string `json:"-" gorm:"type:varchar(64)"`
	Model        string `json:"model" gorm:"type:varchar(200)"`
	Count        int    `json:"-"`
	Status       string `json:"status" gorm:"type:varchar(24);index"`
	RequestID    string `json:"request_id" gorm:"type:varchar(100)"`
	HTTPStatus   int    `json:"http_status,omitempty"`
	Error        string `json:"message,omitempty" gorm:"type:varchar(200)"`
	QuoteLocked  bool   `json:"-"`
	QuoteJSON    string `json:"-" gorm:"type:text"`
	CreatedAt    int64  `json:"created_at"`
	StartedAt    int64  `json:"-"`
	ExpiresAt    int64  `json:"expires_at" gorm:"index"`
}

func GetAPIImageTask(user, token int, id string) (*APIImageTask, error) {
	var task APIImageTask
	err := DB.Where("id = ? AND user_id = ? AND token_id = ?", id, user, token).First(&task).Error
	return &task, err
}

// The api_image_submission unique index makes idempotency atomic, so submits
// do not take the shared drawing queue lock.
func CreateAPIImageTask(task *APIImageTask) (*APIImageTask, error) {
	existing, err := findAPIImageSubmission(task)
	if err == nil {
		return existing, nil
	}
	if !errors.Is(err, gorm.ErrRecordNotFound) {
		return task, err
	}
	if err := DB.Create(task).Error; err != nil {
		// A concurrent submit with the same key may have won the unique index.
		if existing, findErr := findAPIImageSubmission(task); findErr == nil {
			return existing, nil
		} else if !errors.Is(findErr, gorm.ErrRecordNotFound) {
			return task, findErr
		}
		return task, err
	}
	return task, nil
}

func findAPIImageSubmission(task *APIImageTask) (*APIImageTask, error) {
	var existing APIImageTask
	err := DB.Where("user_id = ? AND token_id = ? AND submission_id = ?", task.UserID, task.TokenID, task.SubmissionID).First(&existing).Error
	if err != nil {
		return task, err
	}
	if existing.RequestHash != task.RequestHash {
		return task, errors.New("Idempotency key already used for another request")
	}
	*task = existing
	return task, nil
}

// ReapAPIImageTasks marks interrupted and expired tasks. Workers run it once per
// tick rather than once per claim, since it scans every running task.
func ReapAPIImageTasks(now int64) error {
	// A process may have died AFTER upstream generation or billing. Never replay.
	if err := DB.Model(&APIImageTask{}).Where("status = ? AND started_at < ?", "running", now-25*60).Updates(map[string]any{"status": "unknown", "error": "Worker interrupted; check usage before submitting another task"}).Error; err != nil {
		return err
	}
	return DB.Model(&APIImageTask{}).Where("status = ? AND expires_at <= ?", "queued", now).Updates(map[string]any{"status": "expired"}).Error
}

func ClaimAPIImageTask(now int64) (*APIImageTask, error) {
	if err := ReapAPIImageTasks(now); err != nil {
		return nil, err
	}
	return ClaimNextAPIImageTask(now)
}

// ClaimNextAPIImageTask moves the oldest queued task to running. Admission and
// dispatch add no concurrency ceiling. The conditional update is the claim:
// only one dispatcher can move a task out of "queued", so concurrent
// dispatchers never send the same paid request twice, without a global lock.
func ClaimNextAPIImageTask(now int64) (*APIImageTask, error) {
	for attempt := 0; attempt < 8; attempt++ {
		var task APIImageTask
		err := DB.Where("status = ? AND expires_at > ?", "queued", now).Order("created_at ASC, id ASC").First(&task).Error
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, nil
		}
		if err != nil {
			return nil, err
		}
		result := DB.Model(&APIImageTask{}).Where("id = ? AND status = ? AND expires_at > ?", task.ID, "queued", now).Updates(map[string]any{"status": "running", "started_at": now})
		if result.Error != nil {
			return nil, result.Error
		}
		if result.RowsAffected == 1 {
			task.Status, task.StartedAt = "running", now
			return &task, nil
		}
	}
	// Another dispatcher kept winning; the next tick picks up what is left.
	return nil, nil
}

// Saved results are ~3 MB each; clients fetch them right after polling sees
// completion, so they are kept briefly. The row stays as an idempotency
// tombstone until the 6-day purge.
const APIImageResultLifetime = int64(10 * 60)

func FinishAPIImageTask(task *APIImageTask) error {
	task.ExpiresAt = time.Now().Unix() + APIImageResultLifetime
	return DB.Model(&APIImageTask{}).Where("id = ? AND status = ?", task.ID, "running").Updates(map[string]any{"status": task.Status, "http_status": task.HTTPStatus, "error": task.Error, "expires_at": task.ExpiresAt}).Error
}
