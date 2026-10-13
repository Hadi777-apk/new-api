package controller

import (
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/middleware"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/relaykit/types"
	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/setting/operation_setting"
	"github.com/QuantumNous/new-api/setting/ratio_setting"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestImageDeliveryFailureRefundsWithoutRegenerating(t *testing.T) {
	for _, test := range []struct {
		name string
		body func([]byte) string
	}{
		{"unavailable URL", func(_ []byte) string {
			return `{"data":[{"url":"http://127.0.0.1:1/unavailable"}]}`
		}},
		{"invalid native Base64", func(_ []byte) string {
			return `{"data":[{"b64_json":"!!!invalid-base64!!!"}]}`
		}},
		{"truncated PNG after valid header", func(png []byte) string {
			return `{"data":[{"b64_json":"` + base64.StdEncoding.EncodeToString(png[:33]) + `"}]}`
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, png := setupDrawingTests(t)
			const initialQuota = 1_000_000
			// Both balances stay below the trust threshold so the test exercises a refund.
			require.NoError(t, model.DB.Model(&model.User{}).Where("id = ?", 81001).Update("quota", initialQuota).Error)
			oldRate, oldRetries := operation_setting.USDExchangeRate, common.RetryTimes
			oldPrices := ratio_setting.ModelPrice2JSONString()
			require.NoError(t, ratio_setting.UpdateModelPriceByJSONString(`{"gpt-image-2":0.03}`))
			operation_setting.USDExchangeRate, common.RetryTimes = 1, 2
			t.Cleanup(func() {
				operation_setting.USDExchangeRate, common.RetryTimes = oldRate, oldRetries
				require.NoError(t, ratio_setting.UpdateModelPriceByJSONString(oldPrices))
			})
			_, err := model.UpdateAgentProfile(81001, 1, 2, true, 0, false)
			require.NoError(t, err)
			token := model.Token{UserId: 81001, Key: strings.Repeat("d", 48), Status: 1, RemainQuota: initialQuota, ExpiredTime: -1, Group: "default"}
			require.NoError(t, model.DB.Create(&token).Error)
			var calls atomic.Int32
			var walletAtUpstream, tokenAtUpstream atomic.Int64
			var quotaReadFailed atomic.Bool
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				calls.Add(1)
				// Read the backing database: setupDrawingTests disables Redis and batch updates.
				walletQuota, walletErr := model.GetUserQuota(81001, true)
				var saved model.Token
				tokenErr := model.DB.First(&saved, token.Id).Error
				if walletErr != nil || tokenErr != nil {
					quotaReadFailed.Store(true)
				} else {
					walletAtUpstream.Store(int64(walletQuota))
					tokenAtUpstream.Store(int64(saved.RemainQuota))
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(test.body(png)))
			}))
			defer upstream.Close()
			service.InitHttpClient()
			channel := model.Channel{Type: 1, Key: "fixture", Name: "image-delivery", BaseURL: &upstream.URL, Models: "gpt-image-2", Group: "default", Status: 1}
			require.NoError(t, channel.Insert())
			r := gin.New()
			r.Use(middleware.BodyStorageCleanup())
			r.POST("/v1/images/generations", middleware.TokenAuth(), middleware.Distribute(), func(c *gin.Context) { Relay(c, types.RelayFormatOpenAIImage) })
			req := httptest.NewRequest("POST", "/v1/images/generations", strings.NewReader(`{"model":"gpt-image-2","prompt":"delivery failure test","n":1}`))
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Authorization", "Bearer sk-"+token.Key)
			response := httptest.NewRecorder()
			r.ServeHTTP(response, req)
			assert.Equal(t, http.StatusFailedDependency, response.Code, response.Body.String())
			assert.Contains(t, response.Body.String(), "image_delivery_failed")
			assert.Equal(t, "false", response.Header().Get("x-should-retry"))
			require.EqualValues(t, 1, calls.Load())
			require.False(t, quotaReadFailed.Load(), "upstream quota snapshot must be available")
			require.GreaterOrEqual(t, walletAtUpstream.Load(), int64(0))
			require.Less(t, walletAtUpstream.Load(), int64(initialQuota), "wallet must actually be preconsumed")
			require.GreaterOrEqual(t, tokenAtUpstream.Load(), int64(0))
			require.Less(t, tokenAtUpstream.Load(), int64(initialQuota), "token must actually be preconsumed")
			require.Eventually(t, func() bool {
				walletQuota, walletErr := model.GetUserQuota(81001, true)
				var saved model.Token
				tokenErr := model.DB.First(&saved, token.Id).Error
				return walletErr == nil && tokenErr == nil && walletQuota == initialQuota && saved.RemainQuota == initialQuota
			}, 2*time.Second, 10*time.Millisecond)
		})
	}
}
