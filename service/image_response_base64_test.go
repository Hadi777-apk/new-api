package service

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"hash/crc32"
	"image"
	"image/color"
	"image/gif"
	"image/jpeg"
	"image/png"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/setting/system_setting"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestImageBase64PreservesNativePayloadWithoutFetchingURL(t *testing.T) {
	var buf bytes.Buffer
	require.NoError(t, png.Encode(&buf, image.NewGray(image.Rect(0, 0, 2, 3))))
	encoded := base64.StdEncoding.EncodeToString(buf.Bytes())
	// Valid Base64 line breaks and the original JSON body remain unchanged.
	encoded = encoded[:20] + "\r\n" + encoded[20:]
	body, err := common.Marshal(map[string]any{"data": []map[string]any{{"b64_json": encoded, "url": "http://127.0.0.1:1/unused"}}, "vendor": "keep"})
	require.NoError(t, err)
	result, err := EnsureImageBase64Response(context.Background(), body)
	require.NoError(t, err)
	assert.Equal(t, string(body), string(result))
}

func TestImageBase64RejectsEmptyImageList(t *testing.T) {
	for _, test := range []struct {
		name     string
		validate func(context.Context, []byte) ([]byte, error)
	}{
		{"public", EnsureImageBase64Response},
		{"private spool", ValidateImageNativeBase64Response},
	} {
		t.Run(test.name, func(t *testing.T) {
			result, err := test.validate(context.Background(), []byte(`{"data":[],"usage":{"total_tokens":20}}`))
			require.ErrorContains(t, err, "no images")
			assert.Nil(t, result)
		})
	}
}

func TestImageBase64RejectsInvalidOrPartialDownloads(t *testing.T) {
	setting := system_setting.GetFetchSetting()
	old := *setting
	*setting = system_setting.FetchSetting{EnableSSRFProtection: true, AllowPrivateIp: true, AllowedPorts: []string{"1-65535"}}
	t.Cleanup(func() { *setting = old })
	InitHttpClient()
	for _, status := range []int{200, 206, 403} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(status)
				_, _ = w.Write([]byte("<html>not an image</html>"))
			}))
			defer server.Close()
			result, err := EnsureImageBase64Response(context.Background(), []byte(fmt.Sprintf(`{"data":[{"url":%q}]}`, server.URL+"?secret=hidden")))
			require.Error(t, err)
			assert.Nil(t, result)
			assert.NotContains(t, err.Error(), "hidden")
		})
	}
}

func TestImageBase64ConvertsMixedImagesAndHonorsReadLimit(t *testing.T) {
	setting := system_setting.GetFetchSetting()
	old := *setting
	*setting = system_setting.FetchSetting{EnableSSRFProtection: true, AllowPrivateIp: true, AllowedPorts: []string{"1-65535"}}
	t.Cleanup(func() { *setting = old })
	InitHttpClient()
	var buf bytes.Buffer
	require.NoError(t, png.Encode(&buf, image.NewGray(image.Rect(0, 0, 2, 3))))
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		w.(http.Flusher).Flush()
		_, _ = w.Write(buf.Bytes())
	}))
	defer server.Close()
	encoded := base64.StdEncoding.EncodeToString(buf.Bytes())
	body := []byte(fmt.Sprintf(`{"data":[{"b64_json":%q},{"url":%q,"revised_prompt":"original"}],"usage":{"total_tokens":20}}`, encoded, server.URL))
	result, err := EnsureImageBase64Response(context.Background(), body)
	require.NoError(t, err)
	assert.Equal(t, encoded, gjson.GetBytes(result, "data.0.b64_json").String())
	assert.Equal(t, base64.StdEncoding.EncodeToString(buf.Bytes()), gjson.GetBytes(result, "data.1.b64_json").String())
	assert.Equal(t, "original", gjson.GetBytes(result, "data.1.revised_prompt").String())
	assert.EqualValues(t, 20, gjson.GetBytes(result, "usage.total_tokens").Int())
	_, err = downloadImageForBase64(context.Background(), server.URL, 16)
	require.ErrorContains(t, err, "size limit")
}

func TestImageBase64RejectsInvalidNativeImagesWithoutUsingURL(t *testing.T) {
	var buf bytes.Buffer
	require.NoError(t, png.Encode(&buf, image.NewGray(image.Rect(0, 0, 2, 3))))
	for _, test := range []struct {
		name, encoded, message string
	}{
		{"invalid Base64", "not-base64!", "invalid image Base64"},
		{"non-image", base64.StdEncoding.EncodeToString([]byte("<html>not an image</html>")), "not a supported image"},
		{"header without pixels", base64.StdEncoding.EncodeToString(buf.Bytes()[:33]), "incomplete or invalid image"},
		{"invalid data URL", "data:image/png," + base64.StdEncoding.EncodeToString(buf.Bytes()), "data URL"},
		{"non-image data URL", "data:text/plain;base64," + base64.StdEncoding.EncodeToString(buf.Bytes()), "data URL"},
	} {
		t.Run(test.name, func(t *testing.T) {
			body, err := common.Marshal(map[string]any{"data": []map[string]any{{"b64_json": test.encoded, "url": "http://127.0.0.1:1/unused?secret=hidden"}}})
			require.NoError(t, err)
			result, err := EnsureImageBase64Response(context.Background(), body)
			require.ErrorContains(t, err, test.message)
			assert.Nil(t, result)
			assert.NotContains(t, err.Error(), "hidden")
		})
	}
}

func TestImageBase64DataURLUsesActualImageFormatAndKeepsMixedMetadata(t *testing.T) {
	var first, second bytes.Buffer
	require.NoError(t, png.Encode(&first, image.NewGray(image.Rect(0, 0, 2, 3))))
	require.NoError(t, jpeg.Encode(&second, image.NewGray(image.Rect(0, 0, 4, 5)), nil))
	firstEncoded := base64.StdEncoding.EncodeToString(first.Bytes())
	secondEncoded := base64.StdEncoding.EncodeToString(second.Bytes())
	body, err := common.Marshal(map[string]any{"size": "1024x1024", "output_format": "jpeg", "data": []map[string]any{
		{"b64_json": "data:image/jpeg;base64," + firstEncoded, "vendor": "keep"},
		{"b64_json": secondEncoded},
	}, "usage": map[string]int{"total_tokens": 20}})
	require.NoError(t, err)
	result, err := EnsureImageBase64Response(context.Background(), body)
	require.NoError(t, err)
	result = NormalizeImageJSONResponse(context.Background(), result)
	assert.Equal(t, firstEncoded, gjson.GetBytes(result, "data.0.b64_json").String())
	assert.Equal(t, secondEncoded, gjson.GetBytes(result, "data.1.b64_json").String())
	assert.Equal(t, "2x3", gjson.GetBytes(result, "data.0.size").String())
	assert.Equal(t, "png", gjson.GetBytes(result, "data.0.output_format").String())
	assert.Equal(t, "4x5", gjson.GetBytes(result, "data.1.size").String())
	assert.Equal(t, "jpeg", gjson.GetBytes(result, "data.1.output_format").String())
	assert.Equal(t, "null", gjson.GetBytes(result, "size").Raw)
	assert.Equal(t, "null", gjson.GetBytes(result, "output_format").Raw)
	assert.Equal(t, "keep", gjson.GetBytes(result, "data.0.vendor").String())
	assert.EqualValues(t, 20, gjson.GetBytes(result, "usage.total_tokens").Int())
}

func TestImageBase64NativeSizeLimits(t *testing.T) {
	var buf bytes.Buffer
	require.NoError(t, png.Encode(&buf, image.NewGray(image.Rect(0, 0, 2, 3))))
	t.Run("single file", func(t *testing.T) {
		oversized := make([]byte, MaxDrawingBytes+1)
		copy(oversized, buf.Bytes())
		body, err := common.Marshal(map[string]any{"data": []map[string]any{{"b64_json": base64.StdEncoding.EncodeToString(oversized)}}})
		require.NoError(t, err)
		result, err := EnsureImageBase64Response(context.Background(), body)
		require.ErrorContains(t, err, "size limit")
		assert.Nil(t, result)
	})
	t.Run("total Base64", func(t *testing.T) {
		padded := make([]byte, 400<<10)
		copy(padded, buf.Bytes())
		encoded := base64.StdEncoding.EncodeToString(padded)
		items := make([]map[string]string, 100)
		for i := range items {
			items[i] = map[string]string{"b64_json": encoded}
		}
		body, err := common.Marshal(map[string]any{"data": items})
		require.NoError(t, err)
		result, err := EnsureImageBase64Response(context.Background(), body)
		require.ErrorContains(t, err, "Base64 size limit")
		assert.Nil(t, result)
	})
	// Reject oversized Base64 before allocating its decoded bytes.
	t.Run("oversized encoding", func(t *testing.T) {
		body := []byte(`{"data":[{"b64_json":"` + strings.Repeat("A", int(MaxDrawingResponseBytes)+4) + `"}]}`)
		result, err := EnsureImageBase64Response(context.Background(), body)
		require.ErrorContains(t, err, "Base64 size limit")
		assert.Nil(t, result)
	})
}

func TestImageBase64RejectsUnsafeDecodedDimensionsBeforePixels(t *testing.T) {
	var buf bytes.Buffer
	require.NoError(t, png.Encode(&buf, image.NewGray(image.Rect(0, 0, 2, 3))))
	data := append([]byte(nil), buf.Bytes()[:33]...)
	binary.BigEndian.PutUint32(data[16:20], 8192)
	binary.BigEndian.PutUint32(data[20:24], 8192)
	binary.BigEndian.PutUint32(data[29:33], crc32.ChecksumIEEE(data[12:29]))
	config, _, err := image.DecodeConfig(bytes.NewReader(data))
	require.NoError(t, err)
	require.Equal(t, 8192, config.Width)
	body, err := common.Marshal(map[string]any{"data": []map[string]any{{"b64_json": base64.StdEncoding.EncodeToString(data)}}})
	require.NoError(t, err)
	result, err := EnsureImageBase64Response(context.Background(), body)
	require.ErrorContains(t, err, "decoding limit")
	assert.Nil(t, result)
}

func TestImageBase64ValidatesWebPAndGIFImages(t *testing.T) {
	var animated bytes.Buffer
	require.NoError(t, gif.Encode(&animated, image.NewPaletted(image.Rect(0, 0, 2, 3), color.Palette{color.Black, color.White}), nil))
	// Small lossless and alpha WebP fixtures encoded from a solid 2x3 image.
	for _, encoded := range []string{
		"UklGRh4AAABXRUJQVlA4TBEAAAAvAYAAAAdQlFKUp/+BiOh/AAA=",
		"UklGRlIAAABXRUJQVlA4WAoAAAAQAAAAAQAAAgAAQUxQSAcAAAAAgICAgICAAFZQOCAkAAAAUAEAnQEqAgADAAFAJiUAToAoAAD++onfkdhDdhy64ubIbwAA",
		base64.StdEncoding.EncodeToString(animated.Bytes()),
	} {
		body, err := common.Marshal(map[string]any{"data": []map[string]any{{"b64_json": encoded}}})
		require.NoError(t, err)
		result, err := EnsureImageBase64Response(context.Background(), body)
		require.NoError(t, err)
		assert.Equal(t, body, result)
		result = NormalizeImageJSONResponse(context.Background(), result)
		assert.Equal(t, "2x3", gjson.GetBytes(result, "size").String())
	}
}

func TestImageBase64RejectsWebPFrameLargerThanCanvas(t *testing.T) {
	// A small VP8X canvas must not conceal an 8192x8192 encoded frame.
	lossless := make([]byte, 5)
	lossless[0] = 0x2f
	binary.LittleEndian.PutUint32(lossless[1:], 8191|8191<<14)
	for _, test := range []struct {
		chunk string
		frame []byte
	}{
		{"VP8 ", []byte{0x10, 0, 0, 0x9d, 0x01, 0x2a, 0, 0x20, 0, 0x20}},
		{"VP8L", lossless},
	} {
		t.Run(test.chunk, func(t *testing.T) {
			data := make([]byte, 38+(len(test.frame)+1)&^1)
			copy(data, "RIFF")
			binary.LittleEndian.PutUint32(data[4:8], uint32(len(data)-8))
			copy(data[8:16], "WEBPVP8X")
			binary.LittleEndian.PutUint32(data[16:20], 10)
			copy(data[30:34], test.chunk)
			binary.LittleEndian.PutUint32(data[34:38], uint32(len(test.frame)))
			copy(data[38:], test.frame)
			config, _, err := image.DecodeConfig(bytes.NewReader(data))
			require.NoError(t, err)
			require.Equal(t, 1, config.Width)
			require.Equal(t, 1, config.Height)
			body, err := common.Marshal(map[string]any{"data": []map[string]any{{"b64_json": base64.StdEncoding.EncodeToString(data)}}})
			require.NoError(t, err)
			result, err := EnsureImageBase64Response(context.Background(), body)
			require.ErrorContains(t, err, "frame dimensions")
			assert.Nil(t, result)
		})
	}
}

func TestImageBase64RejectsCancelledOrMissingImages(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := EnsureImageBase64Response(ctx, []byte(`{"data":[{"url":"https://example.com/image.png"}]}`))
	require.ErrorContains(t, err, "cancelled")
	_, err = EnsureImageBase64Response(context.Background(), []byte(`{"data":[{}]}`))
	require.ErrorContains(t, err, "neither Base64")
	var buf bytes.Buffer
	require.NoError(t, png.Encode(&buf, image.NewGray(image.Rect(0, 0, 2, 3))))
	body, err := common.Marshal(map[string]any{"data": []map[string]any{{"b64_json": base64.StdEncoding.EncodeToString(buf.Bytes())}}})
	require.NoError(t, err)
	_, err = EnsureImageBase64Response(ctx, body)
	require.ErrorContains(t, err, "cancelled")
}

func TestImagePrivateNativeValidationKeepsRecoverableURLs(t *testing.T) {
	var buf bytes.Buffer
	require.NoError(t, png.Encode(&buf, image.NewGray(image.Rect(0, 0, 2, 3))))
	encoded := base64.StdEncoding.EncodeToString(buf.Bytes())
	body, err := common.Marshal(map[string]any{"data": []map[string]any{
		{"b64_json": "data:image/jpeg;base64," + encoded},
		{"url": "http://127.0.0.1:1/recover-later?secret=hidden"},
	}, "usage": map[string]int{"total_tokens": 20}})
	require.NoError(t, err)
	result, err := ValidateImageNativeBase64Response(context.Background(), body)
	require.NoError(t, err)
	assert.Equal(t, encoded, gjson.GetBytes(result, "data.0.b64_json").String())
	assert.Equal(t, "http://127.0.0.1:1/recover-later?secret=hidden", gjson.GetBytes(result, "data.1.url").String())
	assert.EqualValues(t, 20, gjson.GetBytes(result, "usage.total_tokens").Int())
	// A URL-only spool must remain available even when the download is down.
	urlOnly := []byte(`{"data":[{"url":"http://127.0.0.1:1/recover-later"}]}`)
	result, err = ValidateImageNativeBase64Response(context.Background(), urlOnly)
	require.NoError(t, err)
	assert.Equal(t, urlOnly, result)
	result, err = ValidateImageNativeBase64Response(context.Background(), []byte(`{"data":[{"b64_json":"invalid!","url":"http://127.0.0.1:1/recover-later"}]}`))
	require.ErrorContains(t, err, "invalid image Base64")
	assert.Nil(t, result)
}
