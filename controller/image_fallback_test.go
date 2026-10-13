package controller

import (
	"encoding/base64"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/relaykit/types"
	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/setting/operation_setting"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestImageFallbackRequiresExplicitRetryableFailure(t *testing.T) {
	setupDrawingTests(t)
	main := model.Channel{Type: 1, Key: "fixture", Name: "main", Models: "gpt-image-2.5", Group: "default", Status: 1, Priority: common.GetPointer(int64(100))}
	backup := model.Channel{Type: 1, Key: "fixture", Name: "backup", Models: "gpt-image-2.5", Group: "default", Status: 1, Priority: common.GetPointer(int64(0))}
	require.NoError(t, main.Insert())
	require.NoError(t, backup.Insert())
	for _, tc := range []struct {
		name                             string
		status                           int
		rejected, delivered, skip, retry bool
	}{
		{"transport uncertainty", 500, false, false, false, false},
		{"timeout", 504, true, false, false, false},
		{"invalid prompt", 400, true, false, false, false},
		{"authentication rejected", 401, true, false, false, true},
		{"rate limited", 429, true, false, false, true},
		{"upstream failed", 502, true, false, false, true},
		{"image download failed", 424, false, false, true, false},
		{"response already started", 502, true, true, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, _ := gin.CreateTestContext(httptest.NewRecorder())
			c.Request = httptest.NewRequest("POST", "/v1/images/generations", nil)
			c.Set("image_safe_fallback_only", true)
			c.Set("image_upstream_rejected", tc.rejected)
			c.Set("channel_id", main.Id)
			c.Set("use_channel", []string{"primary"})
			common.SetContextKey(c, constant.ContextKeyUsingGroup, "default")
			common.SetContextKey(c, constant.ContextKeyOriginalModel, "gpt-image-2.5")
			if tc.delivered {
				c.Writer.WriteHeaderNow()
			}
			var opts []types.NewAPIErrorOptions
			if tc.skip {
				opts = append(opts, types.ErrOptionWithSkipRetry())
			}
			err := types.NewErrorWithStatusCode(errors.New("fixture failure"), types.ErrorCodeDoRequestFailed, tc.status, opts...)
			assert.Equal(t, tc.retry, shouldRetry(c, err, 1))
		})
	}
}

func TestQueuedImageFallbackUsesBackupAndChargesOnce(t *testing.T) {
	for _, edit := range []bool{false, true} {
		name := "generation"
		if edit {
			name = "edit"
		}
		t.Run(name, func(t *testing.T) {
			r, png := setupDrawingTests(t)
			oldRate, oldRetry := operation_setting.USDExchangeRate, common.RetryTimes
			operation_setting.USDExchangeRate, common.RetryTimes = 1, 0
			t.Cleanup(func() { operation_setting.USDExchangeRate, common.RetryTimes = oldRate, oldRetry })
			_, err := model.UpdateAgentProfile(81001, 1, 2, true, 0, false)
			require.NoError(t, err)
			var primaryCalls, backupCalls atomic.Int32
			primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				primaryCalls.Add(1)
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusBadGateway)
				_, _ = w.Write([]byte(`{"error":{"message":"provider unavailable","type":"server_error"}}`))
			}))
			defer primary.Close()
			backup := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				backupCalls.Add(1)
				expected := "/v1/images/generations"
				if edit {
					expected = "/v1/images/edits"
				}
				assert.Equal(t, expected, req.URL.Path)
				if edit {
					require.NoError(t, req.ParseMultipartForm(1<<20))
					assert.Equal(t, "gpt-image-2.5", req.FormValue("model"))
					_, _, err := req.FormFile("image")
					assert.NoError(t, err)
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"data":[{"b64_json":"` + base64.StdEncoding.EncodeToString(png) + `"}]}`))
			}))
			defer backup.Close()
			service.InitHttpClient()
			main := model.Channel{Type: 1, Key: "fixture", Name: "primary", BaseURL: &primary.URL, Models: "gpt-image-2.5", Group: "default", Status: 1, Priority: common.GetPointer(int64(100))}
			fallback := model.Channel{Type: 1, Key: "fixture", Name: "backup", BaseURL: &backup.URL, Models: "gpt-image-2.5", Group: "default", Status: 1, Priority: common.GetPointer(int64(0))}
			require.NoError(t, main.Insert())
			require.NoError(t, fallback.Insert())
			var ref []byte
			if edit {
				ref = png
			}
			res := submitDrawingTestBatch(t, r, drawingSubmission{SubmissionID: uuid.NewString(), Model: "gpt-image-2.5", Group: "default", Ratio: "3:4", Prompt: "fallback fixture"}, ref)
			require.Equal(t, 202, res.Code, res.Body.String())
			item, err := model.ClaimDrawingItem(service.DrawingNow(), 4)
			require.NoError(t, err)
			require.NotNil(t, item)
			executeDrawingItem(item)
			var saved model.DrawingItem
			require.NoError(t, model.DB.First(&saved, "id = ?", item.ID).Error)
			assert.Equal(t, "succeeded", saved.Status, saved.Error)
			assert.Equal(t, fallback.Id, saved.ChannelID)
			assert.EqualValues(t, 1, primaryCalls.Load())
			assert.EqualValues(t, 1, backupCalls.Load())
			user, err := model.GetUserById(81001, false)
			require.NoError(t, err)
			assert.Equal(t, 10_000_000-10_000, user.Quota)
		})
	}
}
