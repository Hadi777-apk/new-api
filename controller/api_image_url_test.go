package controller

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"image"
	"image/png"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestAPIImageURLFileDownload(t *testing.T) {
	t.Setenv("DRAWING_STORAGE_DIR", t.TempDir())
	var imageBytes bytes.Buffer
	require.NoError(t, png.Encode(&imageBytes, image.NewNRGBA(image.Rect(0, 0, 3, 4))))
	body, err := common.Marshal(map[string]any{
		"data": []map[string]any{{"b64_json": base64.StdEncoding.EncodeToString(imageBytes.Bytes())}},
	})
	require.NoError(t, err)
	body = service.NormalizeImageJSONResponse(context.Background(), body)
	result, err := service.FormatImageURLResponse(context.Background(), body, "https://images.example.test")
	require.NoError(t, err)
	location, err := url.Parse(gjson.GetBytes(result, "data.0.url").String())
	require.NoError(t, err)
	require.Equal(t, "images.example.test", location.Host)
	require.True(t, strings.HasPrefix(location.Path, "/v1/images/files/"))

	r := gin.New()
	r.GET("/v1/images/files/:name", GetAPIImageFile)
	r.HEAD("/v1/images/files/:name", GetAPIImageFile)
	for _, method := range []string{http.MethodGet, http.MethodHead} {
		t.Run(method+" without API key", func(t *testing.T) {
			request := httptest.NewRequest(method, location.Path, nil)
			response := httptest.NewRecorder()
			r.ServeHTTP(response, request)
			require.Equal(t, http.StatusOK, response.Code)
			assert.Equal(t, "image/png", response.Header().Get("Content-Type"))
			assert.Equal(t, "no-store", response.Header().Get("Cache-Control"))
			assert.Equal(t, "nosniff", response.Header().Get("X-Content-Type-Options"))
			assert.Equal(t, strconv.Itoa(imageBytes.Len()), response.Header().Get("Content-Length"))
			if method == http.MethodGet {
				assert.Equal(t, sha256.Sum256(imageBytes.Bytes()), sha256.Sum256(response.Body.Bytes()))
			} else {
				assert.Empty(t, response.Body.Bytes())
			}
		})
	}

	for _, test := range []struct {
		name string
		file string
		code int
	}{
		{"expired", fmt.Sprintf("%d-%s.png", time.Now().Unix()-1, strings.Repeat("a", 64)), http.StatusGone},
		{"invalid name", "not-an-image.png", http.StatusNotFound},
		{"missing image", fmt.Sprintf("%d-%s.png", time.Now().Unix()+86400, strings.Repeat("0", 64)), http.StatusNotFound},
	} {
		t.Run(test.name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodGet, "/v1/images/files/"+test.file, nil)
			response := httptest.NewRecorder()
			r.ServeHTTP(response, request)
			assert.Equal(t, test.code, response.Code)
			assert.Empty(t, response.Body.Bytes())
			assert.Equal(t, "no-store", response.Header().Get("Cache-Control"))
			assert.Equal(t, "nosniff", response.Header().Get("X-Content-Type-Options"))
		})
	}
}
