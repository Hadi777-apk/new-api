package controller

import (
	"crypto/sha256"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/middleware"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/relaykit/types"
	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/setting/ratio_setting"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestImageURLRelayChargesOnceAndDownloadsWithoutAuth(t *testing.T) {
	_, png := setupDrawingTests(t)
	const initialQuota = 1_000_000
	require.NoError(t, model.DB.Model(&model.User{}).Where("id = ?", 81001).Update("quota", initialQuota).Error)
	oldPrices := ratio_setting.ModelPrice2JSONString()
	require.NoError(t, ratio_setting.UpdateModelPriceByJSONString(`{"gpt-image-2.5":0.03}`))
	t.Cleanup(func() { require.NoError(t, ratio_setting.UpdateModelPriceByJSONString(oldPrices)) })
	common.OptionMapRWMutex.Lock()
	optionsWereNil := common.OptionMap == nil
	if optionsWereNil {
		common.OptionMap = make(map[string]string)
	}
	oldAddress, existed := common.OptionMap["ServerAddress"]
	common.OptionMap["ServerAddress"] = "https://images.example.test"
	common.OptionMapRWMutex.Unlock()
	t.Cleanup(func() {
		common.OptionMapRWMutex.Lock()
		defer common.OptionMapRWMutex.Unlock()
		if optionsWereNil {
			common.OptionMap = nil
		} else if existed {
			common.OptionMap["ServerAddress"] = oldAddress
		} else {
			delete(common.OptionMap, "ServerAddress")
		}
	})
	token := model.Token{UserId: 81001, Key: strings.Repeat("u", 48), Status: 1, RemainQuota: initialQuota, ExpiredTime: -1, Group: "default"}
	require.NoError(t, model.DB.Create(&token).Error)
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		var input map[string]any
		require.NoError(t, common.DecodeJson(r.Body, &input))
		assert.Equal(t, "b64_json", input["response_format"])
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"b64_json":"` + base64.StdEncoding.EncodeToString(png) + `"}],"usage":{"input_tokens":7,"output_tokens":3,"total_tokens":10}}`))
	}))
	defer upstream.Close()
	service.InitHttpClient()
	channel := model.Channel{Type: 1, Key: "fixture", Name: "image-url", BaseURL: &upstream.URL, Models: "gpt-image-2.5", Group: "default", Status: 1}
	require.NoError(t, channel.Insert())
	r := gin.New()
	r.Use(middleware.BodyStorageCleanup())
	r.POST("/v1/images/generations", middleware.TokenAuth(), middleware.Distribute(), func(c *gin.Context) { Relay(c, types.RelayFormatOpenAIImage) })
	r.GET("/v1/images/files/:name", GetAPIImageFile)
	req := httptest.NewRequest(http.MethodPost, "/v1/images/generations", strings.NewReader(`{"model":"gpt-image-2.5","prompt":"URL delivery fixture","n":1,"response_format":"url"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer sk-"+token.Key)
	response := httptest.NewRecorder()
	r.ServeHTTP(response, req)
	require.Equal(t, http.StatusOK, response.Code, response.Body.String())
	assert.False(t, gjson.Get(response.Body.String(), "data.0.b64_json").Exists())
	assert.Equal(t, "3x4", gjson.Get(response.Body.String(), "data.0.size").String())
	assert.EqualValues(t, 10, gjson.Get(response.Body.String(), "usage.total_tokens").Int())
	location, err := url.Parse(gjson.Get(response.Body.String(), "data.0.url").String())
	require.NoError(t, err)
	require.Equal(t, "images.example.test", location.Host)
	var finalQuota int
	var saved model.Token
	require.Eventually(t, func() bool {
		var readErr error
		finalQuota, readErr = model.GetUserQuota(81001, true)
		return readErr == nil && model.DB.First(&saved, token.Id).Error == nil && finalQuota < initialQuota && saved.RemainQuota == finalQuota
	}, 2*time.Second, 10*time.Millisecond)
	assert.Equal(t, initialQuota-15000, finalQuota)
	for range 2 {
		download := httptest.NewRecorder()
		r.ServeHTTP(download, httptest.NewRequest(http.MethodGet, location.Path, nil))
		require.Equal(t, http.StatusOK, download.Code)
		assert.Equal(t, sha256.Sum256(png), sha256.Sum256(download.Body.Bytes()))
	}
	afterDownload, err := model.GetUserQuota(81001, true)
	require.NoError(t, err)
	assert.Equal(t, finalQuota, afterDownload)
	assert.EqualValues(t, 1, calls.Load())
}
