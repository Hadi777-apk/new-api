package openai

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"image"
	"image/png"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	relayconstant "github.com/QuantumNous/new-api/relay/constant"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/setting/system_setting"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestImageURLRequestUsesBase64Upstream(t *testing.T) {
	t.Run("JSON generation preserves client response format", func(t *testing.T) {
		c, _, _, info := newImageTestContext(t, "", "application/json", false)
		request := dto.ImageRequest{Model: "gpt-image-1", Prompt: "image prompt", ResponseFormat: "url"}
		info.Request = &request
		info.RelayMode = relayconstant.RelayModeImagesGenerations
		converted, err := (&Adaptor{}).ConvertImageRequest(c, info, request)
		require.NoError(t, err)
		upstreamRequest, ok := converted.(dto.ImageRequest)
		require.True(t, ok)
		assert.Equal(t, "b64_json", upstreamRequest.ResponseFormat)
		assert.Equal(t, request.Model, upstreamRequest.Model)
		assert.Equal(t, request.Prompt, upstreamRequest.Prompt)
		assert.Equal(t, "url", info.Request.(*dto.ImageRequest).ResponseFormat)
	})

	t.Run("multipart edit preserves image", func(t *testing.T) {
		var imageBytes bytes.Buffer
		require.NoError(t, png.Encode(&imageBytes, image.NewNRGBA(image.Rect(0, 0, 3, 4))))
		var body bytes.Buffer
		writer := multipart.NewWriter(&body)
		require.NoError(t, writer.WriteField("model", "gpt-image-1"))
		require.NoError(t, writer.WriteField("prompt", "edit image"))
		require.NoError(t, writer.WriteField("response_format", "url"))
		part, err := writer.CreateFormFile("image", "original.png")
		require.NoError(t, err)
		_, err = part.Write(imageBytes.Bytes())
		require.NoError(t, err)
		require.NoError(t, writer.Close())
		c, _ := gin.CreateTestContext(httptest.NewRecorder())
		c.Request = httptest.NewRequest(http.MethodPost, "/v1/images/edits", &body)
		c.Request.Header.Set("Content-Type", writer.FormDataContentType())
		require.NoError(t, c.Request.ParseMultipartForm(32<<20))
		t.Cleanup(func() { _ = c.Request.MultipartForm.RemoveAll() })
		request := dto.ImageRequest{Model: "gpt-image-1", Prompt: "edit image", ResponseFormat: "url"}
		info := &relaycommon.RelayInfo{RelayMode: relayconstant.RelayModeImagesEdits, Request: &request}
		converted, err := (&Adaptor{}).ConvertImageRequest(c, info, request)
		require.NoError(t, err)
		convertedBody, ok := converted.(*bytes.Buffer)
		require.True(t, ok)
		replayed := httptest.NewRequest(http.MethodPost, "/v1/images/edits", bytes.NewReader(convertedBody.Bytes()))
		replayed.Header.Set("Content-Type", c.Request.Header.Get("Content-Type"))
		require.NoError(t, replayed.ParseMultipartForm(32<<20))
		t.Cleanup(func() { _ = replayed.MultipartForm.RemoveAll() })
		assert.Equal(t, "b64_json", replayed.PostForm.Get("response_format"))
		assert.Equal(t, request.Model, replayed.PostForm.Get("model"))
		assert.Equal(t, request.Prompt, replayed.PostForm.Get("prompt"))
		assert.Equal(t, "url", info.Request.(*dto.ImageRequest).ResponseFormat)
		require.Len(t, replayed.MultipartForm.File["image"], 1)
		file, err := replayed.MultipartForm.File["image"][0].Open()
		require.NoError(t, err)
		defer file.Close()
		imageData, err := io.ReadAll(file)
		require.NoError(t, err)
		assert.Equal(t, sha256.Sum256(imageBytes.Bytes()), sha256.Sum256(imageData))
	})
}

func TestImageResponseFormatURLPreservesImageAndMetadata(t *testing.T) {
	t.Setenv("DRAWING_STORAGE_DIR", t.TempDir())
	common.OptionMapRWMutex.Lock()
	wasNil := common.OptionMap == nil
	if wasNil {
		common.OptionMap = make(map[string]string)
	}
	oldAddress, existed := common.OptionMap["ServerAddress"]
	common.OptionMap["ServerAddress"] = "https://images.example.test/"
	common.OptionMapRWMutex.Unlock()
	t.Cleanup(func() {
		common.OptionMapRWMutex.Lock()
		defer common.OptionMapRWMutex.Unlock()
		if wasNil {
			common.OptionMap = nil
		} else if existed {
			common.OptionMap["ServerAddress"] = oldAddress
		} else {
			delete(common.OptionMap, "ServerAddress")
		}
	})
	setting := system_setting.GetFetchSetting()
	oldFetch := *setting
	*setting = system_setting.FetchSetting{EnableSSRFProtection: true, AllowPrivateIp: true, AllowedPorts: []string{"1-65535"}}
	t.Cleanup(func() { *setting = oldFetch })
	service.InitHttpClient()

	var imageBytes bytes.Buffer
	require.NoError(t, png.Encode(&imageBytes, image.NewNRGBA(image.Rect(0, 0, 3, 4))))
	var downloads atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		downloads.Add(1)
		assert.Empty(t, r.Header.Get("Authorization"))
		assert.Empty(t, r.Header.Get("Cookie"))
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write(imageBytes.Bytes())
	}))
	defer upstream.Close()
	for _, test := range []struct {
		name string
		item map[string]any
	}{
		{"native Base64", map[string]any{"b64_json": base64.StdEncoding.EncodeToString(imageBytes.Bytes()), "url": "http://127.0.0.1:1/unused"}},
		{"upstream URL", map[string]any{"url": upstream.URL + "/image.png"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			test.item["revised_prompt"] = "upstream revised prompt"
			test.item["custom"] = "preserved"
			body, err := common.Marshal(map[string]any{
				"created":            123,
				"data":               []map[string]any{test.item},
				"quality":            "high",
				"background":         "transparent",
				"upstream_extension": "preserved",
				"usage":              map[string]int{"input_tokens": 11, "output_tokens": 9, "total_tokens": 20},
			})
			require.NoError(t, err)
			c, recorder, response, info := newImageTestContext(t, string(body), "application/json", false)
			info.Request = &dto.ImageRequest{ResponseFormat: "url"}
			usage, apiErr := OpenaiImageHandler(c, info, response)
			require.Nil(t, apiErr)
			require.NotNil(t, usage)
			require.Equal(t, http.StatusOK, recorder.Code)
			result := recorder.Body.Bytes()
			assert.False(t, gjson.GetBytes(result, "data.0.b64_json").Exists())
			assert.Equal(t, gjson.GetBytes(body, "usage").Raw, gjson.GetBytes(result, "usage").Raw)
			assert.EqualValues(t, 123, gjson.GetBytes(result, "created").Int())
			assert.Equal(t, "high", gjson.GetBytes(result, "quality").String())
			assert.Equal(t, "transparent", gjson.GetBytes(result, "background").String())
			assert.Equal(t, "preserved", gjson.GetBytes(result, "upstream_extension").String())
			assert.Equal(t, "preserved", gjson.GetBytes(result, "data.0.custom").String())
			assert.Equal(t, "upstream revised prompt", gjson.GetBytes(result, "data.0.revised_prompt").String())
			assert.Equal(t, "3x4", gjson.GetBytes(result, "size").String())
			assert.Equal(t, "3x4", gjson.GetBytes(result, "data.0.size").String())
			assert.EqualValues(t, 3, gjson.GetBytes(result, "data.0.width").Int())
			assert.EqualValues(t, 4, gjson.GetBytes(result, "data.0.height").Int())
			assert.Equal(t, "png", gjson.GetBytes(result, "output_format").String())
			assert.Equal(t, "png", gjson.GetBytes(result, "data.0.output_format").String())
			assert.EqualValues(t, 11, usage.PromptTokens)
			assert.EqualValues(t, 9, usage.CompletionTokens)
			assert.EqualValues(t, 20, usage.TotalTokens)

			location, err := url.Parse(gjson.GetBytes(result, "data.0.url").String())
			require.NoError(t, err)
			assert.Equal(t, "https", location.Scheme)
			assert.Equal(t, "images.example.test", location.Host)
			require.True(t, strings.HasPrefix(location.Path, "/v1/images/files/"))
			file, err := service.OpenImageURLFile(path.Base(location.Path), time.Now().Unix())
			require.NoError(t, err)
			defer file.Close()
			imageData, err := io.ReadAll(file)
			require.NoError(t, err)
			assert.Equal(t, sha256.Sum256(imageBytes.Bytes()), sha256.Sum256(imageData))
		})
	}
	assert.EqualValues(t, 1, downloads.Load(), "URL generation and metadata must reuse the downloaded image")
}
