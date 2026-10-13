package controller

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
	"gorm.io/gorm"
)

func setupAPIImageTaskReadFixture(t *testing.T) (*gin.Engine, *model.Token, *model.APIImageTask) {
	t.Helper()
	router, token, _ := setupAPIImageTasks(t)
	task := &model.APIImageTask{
		ID: uuid.NewString(), UserID: token.UserId, TokenID: token.Id,
		SubmissionID: uuid.NewString(), RequestHash: "read-test", Model: model.AgentImageModel,
		Count: 1, Status: "queued", CreatedAt: time.Now().Unix(), ExpiresAt: time.Now().Add(time.Hour).Unix(),
	}
	require.NoError(t, model.DB.Create(task).Error)
	return router, token, task
}

func TestAPIImageTaskGetDistinguishesDatabaseFailures(t *testing.T) {
	privateDBError := errors.New("private-db-detail: connection slots unavailable")
	for _, test := range []struct {
		name, lookup, code string
		failure            error
		status             int
	}{
		{"token database failure", "token", "database_unavailable", privateDBError, http.StatusServiceUnavailable},
		{"token no longer exists", "token", "invalid_token", fmt.Errorf("token lookup: %w", gorm.ErrRecordNotFound), http.StatusUnauthorized},
		{"task database failure", "task", "database_unavailable", privateDBError, http.StatusServiceUnavailable},
		{"task no longer exists", "task", "task_not_found", fmt.Errorf("task lookup: %w", gorm.ErrRecordNotFound), http.StatusNotFound},
	} {
		t.Run(test.name, func(t *testing.T) {
			router, token, task := setupAPIImageTaskReadFixture(t)
			db := model.DB
			failures := 0
			callbackName := "test:api-image-task-read-failure"
			require.NoError(t, db.Callback().Query().Before("gorm:query").Register(callbackName, func(tx *gorm.DB) {
				switch destination := tx.Statement.Dest.(type) {
				case *model.Token:
					// Authentication looks up a zero-valued Token by key. Fail
					// only the controller's second lookup by authenticated IDs.
					if test.lookup == "token" && destination.Id == token.Id {
						failures++
						tx.AddError(test.failure)
					}
				case *model.APIImageTask:
					if test.lookup == "task" {
						failures++
						tx.AddError(test.failure)
					}
				}
			}))
			callbackInstalled := true
			t.Cleanup(func() {
				if callbackInstalled {
					require.NoError(t, db.Callback().Query().Remove(callbackName))
				}
			})
			for _, path := range []string{
				"/v1/images/tasks/" + task.ID,
				"/v1/images/tasks/" + task.ID + "/result",
				"/v1/images/tasks/by-submission/" + task.SubmissionID,
			} {
				response := apiTaskCall(t, router, token, http.MethodGet, path, "", "")
				assert.Equal(t, test.status, response.Code, response.Body.String())
				assert.Equal(t, test.code, gjson.GetBytes(response.Body.Bytes(), "error.code").String())
				assert.NotContains(t, response.Body.String(), "private-db-detail")
				if test.status == http.StatusServiceUnavailable {
					assert.Equal(t, "true", response.Header().Get("x-should-retry"))
					assert.Equal(t, "2", response.Header().Get("Retry-After"))
					assert.Contains(t, response.Body.String(), "retry this GET request")
				} else {
					assert.Equal(t, "false", response.Header().Get("x-should-retry"))
					assert.Empty(t, response.Header().Get("Retry-After"))
				}
			}
			require.Equal(t, 3, failures)
			require.NoError(t, db.Callback().Query().Remove(callbackName))
			callbackInstalled = false
			// A read retry recovers the existing task, without creating or
			// executing another image request or changing its token balance.
			recovered := apiTaskCall(t, router, token, http.MethodGet, "/v1/images/tasks/"+task.ID, "", "")
			require.Equal(t, http.StatusOK, recovered.Code, recovered.Body.String())
			assert.Equal(t, task.ID, gjson.GetBytes(recovered.Body.Bytes(), "id").String())
			assert.Equal(t, "queued", gjson.GetBytes(recovered.Body.Bytes(), "status").String())
			var count int64
			require.NoError(t, db.Model(&model.APIImageTask{}).Count(&count).Error)
			assert.EqualValues(t, 1, count)
			savedToken, err := model.GetTokenById(token.Id)
			require.NoError(t, err)
			assert.Equal(t, token.RemainQuota, savedToken.RemainQuota)
		})
	}
}

func TestAPIImageTaskReadKeepsTokenAuthenticationContract(t *testing.T) {
	t.Run("disabled key", func(t *testing.T) {
		router, token, task := setupAPIImageTaskReadFixture(t)
		require.NoError(t, model.DB.Model(token).Update("status", common.TokenStatusDisabled).Error)
		response := apiTaskCall(t, router, token, http.MethodGet, "/v1/images/tasks/"+task.ID, "", "")
		assert.Equal(t, http.StatusUnauthorized, response.Code)
		assert.Empty(t, response.Header().Get("Retry-After"))
	})
	t.Run("unknown key", func(t *testing.T) {
		router, token, task := setupAPIImageTaskReadFixture(t)
		token.Key = strings.Repeat("z", 48)
		response := apiTaskCall(t, router, token, http.MethodGet, "/v1/images/tasks/"+task.ID, "", "")
		assert.Equal(t, http.StatusUnauthorized, response.Code)
		assert.Empty(t, response.Header().Get("Retry-After"))
	})
	t.Run("expired key can recover but cannot submit", func(t *testing.T) {
		router, token, task := setupAPIImageTaskReadFixture(t)
		require.NoError(t, model.DB.Model(token).Update("expired_time", time.Now().Unix()-60).Error)
		submission := apiTaskCall(t, router, token, http.MethodPost, "/v1/images/tasks", uuid.NewString(), `{"model":"gpt-image-2.5","prompt":"must not submit"}`)
		assert.Equal(t, http.StatusUnauthorized, submission.Code)
		assert.Empty(t, submission.Header().Get("Retry-After"))
		recovered := apiTaskCall(t, router, token, http.MethodGet, "/v1/images/tasks/"+task.ID, "", "")
		require.Equal(t, http.StatusOK, recovered.Code, recovered.Body.String())
		assert.Equal(t, task.ID, gjson.GetBytes(recovered.Body.Bytes(), "id").String())
		var count int64
		require.NoError(t, model.DB.Model(&model.APIImageTask{}).Count(&count).Error)
		assert.EqualValues(t, 1, count)
	})
}
