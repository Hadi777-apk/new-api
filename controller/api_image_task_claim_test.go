package controller

import (
	"sync"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/model"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestAPIImageTaskConcurrentClaimsNeverDuplicate(t *testing.T) {
	_, token, _ := setupAPIImageTasks(t)
	now := time.Now().Unix()
	const total = 60
	for i := 0; i < total; i++ {
		require.NoError(t, model.DB.Create(&model.APIImageTask{
			ID: uuid.NewString(), UserID: token.UserId, TokenID: token.Id, SubmissionID: uuid.NewString(),
			RequestHash: "claim-test", Model: model.AgentImageModel, Count: 1, Status: "queued",
			CreatedAt: now + int64(i), ExpiresAt: now + 3600,
		}).Error)
	}
	var mu sync.Mutex
	claimed := map[string]int{}
	var wg sync.WaitGroup
	for worker := 0; worker < 8; worker++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				task, err := model.ClaimNextAPIImageTask(now)
				if err != nil {
					t.Error(err)
					return
				}
				if task == nil {
					return
				}
				assert.Equal(t, "running", task.Status)
				mu.Lock()
				claimed[task.ID]++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	require.Len(t, claimed, total, "every queued task is claimed")
	for id, count := range claimed {
		assert.Equal(t, 1, count, "task %s claimed more than once", id)
	}
	var running int64
	require.NoError(t, model.DB.Model(&model.APIImageTask{}).Where("status = ? AND started_at = ?", "running", now).Count(&running).Error)
	assert.EqualValues(t, total, running)
}

func TestAPIImageTaskResultExpiresTenMinutesAfterCompletion(t *testing.T) {
	router, token, _ := setupAPIImageTasks(t)
	created := time.Now().Unix() - 3600
	task := &model.APIImageTask{
		ID: uuid.NewString(), UserID: token.UserId, TokenID: token.Id, SubmissionID: uuid.NewString(),
		RequestHash: "lifetime-test", Model: model.AgentImageModel, Count: 1, Status: "running",
		CreatedAt: created, StartedAt: created, ExpiresAt: created + 24*3600,
	}
	require.NoError(t, model.DB.Create(task).Error)
	task.Status, task.HTTPStatus = "succeeded", 200
	before := time.Now().Unix()
	require.NoError(t, model.FinishAPIImageTask(task))
	saved, err := model.GetAPIImageTask(token.UserId, token.Id, task.ID)
	require.NoError(t, err)
	assert.Equal(t, "succeeded", saved.Status)
	assert.GreaterOrEqual(t, saved.ExpiresAt, before+model.APIImageResultLifetime)
	assert.LessOrEqual(t, saved.ExpiresAt, time.Now().Unix()+model.APIImageResultLifetime)
	assert.Equal(t, 200, apiTaskCall(t, router, token, "GET", "/v1/images/tasks/"+task.ID, "", "").Code)

	require.NoError(t, model.DB.Model(saved).Update("expires_at", time.Now().Unix()-1).Error)
	assert.Equal(t, 410, apiTaskCall(t, router, token, "GET", "/v1/images/tasks/"+task.ID+"/result", "", "").Code)
	resubmit, err := model.CreateAPIImageTask(&model.APIImageTask{
		ID: uuid.NewString(), UserID: token.UserId, TokenID: token.Id, SubmissionID: task.SubmissionID,
		RequestHash: "lifetime-test", Model: model.AgentImageModel, Status: "queued", CreatedAt: time.Now().Unix(), ExpiresAt: time.Now().Unix() + 3600,
	})
	require.NoError(t, err)
	assert.Equal(t, task.ID, resubmit.ID, "an expired result keeps its idempotency key, so it is never generated again")
}

func TestAPIImageTaskConcurrentSubmitsShareOneTask(t *testing.T) {
	_, token, _ := setupAPIImageTasks(t)
	now := time.Now().Unix()
	submission := uuid.NewString()
	ids := make(chan string, 8)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			task, err := model.CreateAPIImageTask(&model.APIImageTask{
				ID: uuid.NewString(), UserID: token.UserId, TokenID: token.Id, SubmissionID: submission,
				RequestHash: "same-request", Model: model.AgentImageModel, Count: 1, Status: "queued",
				CreatedAt: now, ExpiresAt: now + 3600,
			})
			if assert.NoError(t, err) {
				ids <- task.ID
			}
		}()
	}
	wg.Wait()
	close(ids)
	first := ""
	for id := range ids {
		if first == "" {
			first = id
		}
		assert.Equal(t, first, id, "every submit with one idempotency key returns the same task")
	}
	var count int64
	require.NoError(t, model.DB.Model(&model.APIImageTask{}).Where("submission_id = ?", submission).Count(&count).Error)
	assert.EqualValues(t, 1, count)
	_, err := model.CreateAPIImageTask(&model.APIImageTask{
		ID: uuid.NewString(), UserID: token.UserId, TokenID: token.Id, SubmissionID: submission,
		RequestHash: "different-request", Model: model.AgentImageModel, Status: "queued", CreatedAt: now, ExpiresAt: now + 3600,
	})
	assert.Error(t, err, "a reused idempotency key with another request is rejected")
}
