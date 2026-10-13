package service

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"image"
	"image/png"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/setting/system_setting"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func configureImageDownloadTest(t *testing.T) {
	t.Helper()
	setting := system_setting.GetFetchSetting()
	previous := *setting
	previousClient, previousProtectedClient := httpClient, ssrfProtectedHTTPClient
	previousTimeout := common.RelayTimeout
	t.Cleanup(func() {
		*setting = previous
		httpClient, ssrfProtectedHTTPClient = previousClient, previousProtectedClient
		common.RelayTimeout = previousTimeout
	})
	*setting = system_setting.FetchSetting{EnableSSRFProtection: true, AllowPrivateIp: true, AllowedPorts: []string{"1-65535"}}
	common.RelayTimeout = 0
	InitHttpClient()
}

func imageDownloadPNG(t *testing.T) []byte {
	t.Helper()
	var buffer bytes.Buffer
	encoder := png.Encoder{CompressionLevel: png.NoCompression}
	require.NoError(t, encoder.Encode(&buffer, image.NewNRGBA(image.Rect(0, 0, 32, 32))))
	return buffer.Bytes()
}

func imageDownloadURLBody(t *testing.T, location string) []byte {
	t.Helper()
	body, err := common.Marshal(map[string]any{
		"data":  []map[string]any{{"url": location}},
		"usage": map[string]any{"total_tokens": 20},
	})
	require.NoError(t, err)
	return body
}

func captureImageDownloadLogs(t *testing.T) *bytes.Buffer {
	t.Helper()
	buffer := new(bytes.Buffer)
	common.LogWriterMu.Lock()
	previous := gin.DefaultErrorWriter
	gin.DefaultErrorWriter = buffer
	common.LogWriterMu.Unlock()
	t.Cleanup(func() {
		common.LogWriterMu.Lock()
		gin.DefaultErrorWriter = previous
		common.LogWriterMu.Unlock()
	})
	return buffer
}

func TestImageDownloadRetriesTruncatedHTTPBodyWithoutRegeneration(t *testing.T) {
	for _, mode := range []string{"content_length", "chunked_pixel_data"} {
		t.Run(mode, func(t *testing.T) {
			configureImageDownloadTest(t)
			logs := captureImageDownloadLogs(t)
			original := imageDownloadPNG(t)
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				assert.Equal(t, http.MethodGet, r.Method)
				assert.Empty(t, r.Header.Get("Range"))
				assert.Empty(t, r.Header.Get("Authorization"))
				assert.Empty(t, r.Header.Get("Cookie"))
				if calls.Add(1) == 1 {
					if mode == "content_length" {
						w.Header().Set("Content-Length", strconv.Itoa(len(original)))
					} else {
						w.WriteHeader(http.StatusOK)
						w.(http.Flusher).Flush()
					}
					_, _ = w.Write(original[:len(original)/2])
					return
				}
				_, _ = w.Write(original)
			}))
			defer server.Close()
			location := server.URL + "/private-image-path?signature=hidden-signature"
			ctx := context.WithValue(context.Background(), common.RequestIdKey, "image-download-request")
			result, err := EnsureImageBase64Response(ctx, imageDownloadURLBody(t, location))
			require.NoError(t, err)
			decoded, err := base64.StdEncoding.DecodeString(gjson.GetBytes(result, "data.0.b64_json").String())
			require.NoError(t, err)
			assert.Equal(t, sha256.Sum256(original), sha256.Sum256(decoded))
			result = NormalizeImageJSONResponse(ctx, result)
			assert.Equal(t, "32x32", gjson.GetBytes(result, "data.0.size").String())
			assert.EqualValues(t, 20, gjson.GetBytes(result, "usage.total_tokens").Int())
			assert.EqualValues(t, 2, calls.Load(), "metadata must reuse the recovered image")
			assert.Contains(t, logs.String(), "image-download-request")
			assert.Contains(t, logs.String(), "reason=unexpected_eof")
			assert.Contains(t, logs.String(), "attempt=1")
			assert.Contains(t, logs.String(), "bytes=")
			assert.NotContains(t, logs.String(), "private-image-path")
			assert.NotContains(t, logs.String(), "hidden-signature")
		})
	}
}

func TestImageDownloadStopsAfterTwoIncompleteDownloads(t *testing.T) {
	configureImageDownloadTest(t)
	original := imageDownloadPNG(t)
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Length", strconv.Itoa(len(original)))
		_, _ = w.Write(original[:len(original)/2])
	}))
	defer server.Close()
	result, err := EnsureImageBase64Response(context.Background(), imageDownloadURLBody(t, server.URL))
	require.ErrorContains(t, err, "image 1: image download was incomplete")
	assert.ErrorIs(t, err, io.ErrUnexpectedEOF)
	assert.Nil(t, result)
	assert.EqualValues(t, 2, calls.Load())
}

func TestImageDownloadRetriesClientBodyTimeout(t *testing.T) {
	configureImageDownloadTest(t)
	ssrfProtectedHTTPClient.Timeout = time.Second
	original := imageDownloadPNG(t)
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			w.WriteHeader(http.StatusOK)
			w.(http.Flusher).Flush()
			_, _ = w.Write(original[:32])
			w.(http.Flusher).Flush()
			<-r.Context().Done()
			return
		}
		_, _ = w.Write(original)
	}))
	defer server.Close()
	data, err := downloadImageForBase64(context.Background(), server.URL, MaxDrawingBytes)
	require.NoError(t, err)
	assert.Equal(t, original, data)
	assert.EqualValues(t, 2, calls.Load())
}

func TestImageDownloadUserCancellationDoesNotRetry(t *testing.T) {
	configureImageDownloadTest(t)
	var calls atomic.Int32
	started := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		close(started)
		<-r.Context().Done()
	}))
	defer server.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	result := make(chan error, 1)
	go func() {
		_, err := downloadImageForBase64(ctx, server.URL, MaxDrawingBytes)
		result <- err
	}()
	<-started
	cancel()
	select {
	case err := <-result:
		assert.ErrorIs(t, err, context.Canceled)
		assert.ErrorContains(t, err, "cancelled")
	case <-time.After(2 * time.Second):
		t.Fatal("image download did not stop when its caller cancelled")
	}
	assert.EqualValues(t, 1, calls.Load())
}

func TestImageDownloadCallerDeadlineDoesNotRetry(t *testing.T) {
	configureImageDownloadTest(t)
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	data, err := downloadImageForBase64(ctx, server.URL, MaxDrawingBytes)
	require.ErrorIs(t, err, context.DeadlineExceeded)
	assert.Nil(t, data)
	assert.EqualValues(t, 1, calls.Load())
}

func TestImageDownloadBlockedFetchDoesNotRetry(t *testing.T) {
	configureImageDownloadTest(t)
	system_setting.GetFetchSetting().AllowPrivateIp = false
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
	}))
	defer server.Close()
	data, err := downloadImageForBase64(context.Background(), server.URL, MaxDrawingBytes)
	require.ErrorContains(t, err, "blocked by the fetch policy")
	assert.Nil(t, data)
	assert.Zero(t, calls.Load())
}

func TestImageDownloadPermanentFailuresDoNotRetry(t *testing.T) {
	for _, kind := range []string{"not_found", "declared_size_limit", "streamed_size_limit", "invalid_image"} {
		t.Run(kind, func(t *testing.T) {
			configureImageDownloadTest(t)
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				switch kind {
				case "not_found":
					w.WriteHeader(http.StatusNotFound)
				case "declared_size_limit":
					w.Header().Set("Content-Length", "100")
					_, _ = w.Write([]byte("too large"))
				case "streamed_size_limit":
					w.WriteHeader(http.StatusOK)
					w.(http.Flusher).Flush()
					_, _ = w.Write(make([]byte, 100))
				case "invalid_image":
					_, _ = w.Write([]byte("<html>not an image</html>"))
				}
			}))
			defer server.Close()
			data, err := downloadImageForBase64(context.Background(), server.URL, 32)
			require.Error(t, err)
			assert.Nil(t, data)
			assert.EqualValues(t, 1, calls.Load())
		})
	}
}

func TestImageDownloadRetriesOnlyTransientHTTPStatuses(t *testing.T) {
	for _, status := range []int{408, 429, 502, 503, 504} {
		t.Run(strconv.Itoa(status), func(t *testing.T) {
			configureImageDownloadTest(t)
			original := imageDownloadPNG(t)
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if calls.Add(1) == 1 {
					w.WriteHeader(status)
					return
				}
				_, _ = w.Write(original)
			}))
			defer server.Close()
			data, err := downloadImageForBase64(context.Background(), server.URL, MaxDrawingBytes)
			require.NoError(t, err)
			assert.Equal(t, original, data)
			assert.EqualValues(t, 2, calls.Load())
		})
	}
}

func TestImageDownloadPreservesRequestCauseWithoutLeakingSignedURL(t *testing.T) {
	configureImageDownloadTest(t)
	logs := captureImageDownloadLogs(t)
	ssrfProtectedHTTPClient.Timeout = time.Second
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		<-r.Context().Done()
	}))
	defer server.Close()
	location := server.URL + "/secret-storage-path?signature=hidden-signature"
	data, err := downloadImageForBase64(context.Background(), location, MaxDrawingBytes)
	require.ErrorContains(t, err, "image download failed or timed out")
	assert.Nil(t, data)
	var requestError *url.Error
	assert.True(t, errors.As(err, &requestError), "retry classification must retain the transport cause")
	assert.EqualValues(t, 2, calls.Load())
	assert.Contains(t, logs.String(), "reason=timeout")
	for _, value := range []string{"secret-storage-path", "hidden-signature", location} {
		assert.NotContains(t, err.Error(), value)
		assert.NotContains(t, logs.String(), value)
	}
}
