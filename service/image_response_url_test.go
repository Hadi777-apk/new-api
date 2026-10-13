package service

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"image"
	"image/color"
	"image/gif"
	"image/jpeg"
	"image/png"
	"io"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func imageURLTestPNG(t *testing.T) []byte {
	t.Helper()
	var data bytes.Buffer
	require.NoError(t, png.Encode(&data, image.NewGray(image.Rect(0, 0, 2, 3))))
	return data.Bytes()
}

func TestImageURLPreservesOriginalImageBytesAndMetadata(t *testing.T) {
	t.Setenv("DRAWING_STORAGE_DIR", t.TempDir())
	var jpegData, gifData bytes.Buffer
	require.NoError(t, jpeg.Encode(&jpegData, image.NewGray(image.Rect(0, 0, 2, 3)), nil))
	require.NoError(t, gif.Encode(&gifData, image.NewPaletted(image.Rect(0, 0, 2, 3), color.Palette{color.Black, color.White}), nil))
	webpData, err := base64.StdEncoding.DecodeString("UklGRh4AAABXRUJQVlA4TBEAAAAvAYAAAAdQlFKUp/+BiOh/AAA=")
	require.NoError(t, err)
	for _, fixture := range []struct {
		format string
		data   []byte
	}{
		{"png", imageURLTestPNG(t)}, {"jpeg", jpegData.Bytes()}, {"gif", gifData.Bytes()}, {"webp", webpData},
	} {
		t.Run(fixture.format, func(t *testing.T) {
			body, err := common.Marshal(map[string]any{
				"created": 123, "usage": map[string]int{"total_tokens": 20}, "vendor": map[string]string{"job": "keep"},
				"data": []map[string]any{{"b64_json": base64.StdEncoding.EncodeToString(fixture.data), "url": "https://upstream.invalid/private?credential=hidden", "revised_prompt": "original", "vendor_item": true}},
			})
			require.NoError(t, err)
			body, err = EnsureImageBase64Response(context.Background(), body)
			require.NoError(t, err)
			body = NormalizeImageJSONResponse(context.Background(), body)
			before := time.Now().Unix()
			result, err := FormatImageURLResponse(context.Background(), body, "https://images.example/prefix/")
			require.NoError(t, err)
			assert.False(t, gjson.GetBytes(result, "data.0.b64_json").Exists())
			assert.Equal(t, "2x3", gjson.GetBytes(result, "data.0.size").String())
			assert.Equal(t, "2x3", gjson.GetBytes(result, "size").String())
			assert.Equal(t, fixture.format, gjson.GetBytes(result, "data.0.output_format").String())
			assert.EqualValues(t, 123, gjson.GetBytes(result, "created").Int())
			assert.EqualValues(t, 20, gjson.GetBytes(result, "usage.total_tokens").Int())
			assert.Equal(t, "keep", gjson.GetBytes(result, "vendor.job").String())
			assert.True(t, gjson.GetBytes(result, "data.0.vendor_item").Bool())
			assert.Equal(t, "original", gjson.GetBytes(result, "data.0.revised_prompt").String())
			assert.NotContains(t, string(result), "upstream.invalid")
			assert.NotContains(t, string(result), "hidden")
			expiresAt := gjson.GetBytes(result, "data.0.expires_at").Int()
			assert.GreaterOrEqual(t, expiresAt, before+imageURLLifetime)
			assert.LessOrEqual(t, expiresAt, time.Now().Unix()+imageURLLifetime)
			location, err := url.Parse(gjson.GetBytes(result, "data.0.url").String())
			require.NoError(t, err)
			assert.Equal(t, "images.example", location.Host)
			assert.True(t, strings.HasPrefix(location.Path, "/prefix/v1/images/files/"))
			name := path.Base(location.Path)
			file, err := OpenImageURLFile(name, expiresAt-1)
			require.NoError(t, err)
			stored, err := io.ReadAll(file)
			require.NoError(t, err)
			require.NoError(t, file.Close())
			assert.Equal(t, sha256.Sum256(fixture.data), sha256.Sum256(stored))
			file, err = OpenImageURLFile(name, expiresAt)
			assert.ErrorIs(t, err, ErrImageURLExpired)
			assert.Nil(t, file)
		})
	}
}

func TestImageURLRejectsInvalidFilenames(t *testing.T) {
	t.Setenv("DRAWING_STORAGE_DIR", t.TempDir())
	now := time.Now().Unix()
	validName := fmt.Sprintf("%d-%s.png", now+60, strings.Repeat("a", 64))
	for _, name := range []string{
		"../" + validName, "..\\" + validName, "/" + validName,
		validName + "/../other.png", "0" + validName, strings.Replace(validName, "a", "A", 1),
		strings.TrimSuffix(validName, ".png") + ".svg", strings.TrimSuffix(validName, ".png") + ".html", "1-short.png", "", validName + "?download=1",
	} {
		file, err := OpenImageURLFile(name, now)
		assert.Error(t, err, name)
		assert.NotErrorIs(t, err, ErrImageURLExpired, name)
		assert.Nil(t, file, name)
	}
	file, err := OpenImageURLFile(validName, now)
	assert.ErrorIs(t, err, os.ErrNotExist)
	assert.Nil(t, file)
}

func TestImageURLCleanupExpiresFilesAndAbandonedWrites(t *testing.T) {
	t.Setenv("DRAWING_STORAGE_DIR", t.TempDir())
	now := time.Now().Unix()
	dir := imageURLStorageDir()
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "keep-dir"), 0700))
	expired := fmt.Sprintf("%d-%s.png", now, strings.Repeat("a", 64))
	active := fmt.Sprintf("%d-%s.jpg", now+60, strings.Repeat("b", 64))
	for _, name := range []string{expired, active, "write-abandoned", "write-active", "unrelated.txt"} {
		require.NoError(t, os.WriteFile(filepath.Join(dir, name), []byte("fixture"), 0600))
	}
	old := time.Unix(now-imageURLLifetime, 0)
	require.NoError(t, os.Chtimes(filepath.Join(dir, "write-abandoned"), old, old))
	require.NoError(t, CleanupImageURLFiles(now))
	for _, name := range []string{expired, "write-abandoned"} {
		_, err := os.Stat(filepath.Join(dir, name))
		assert.ErrorIs(t, err, os.ErrNotExist)
	}
	for _, name := range []string{active, "write-active", "unrelated.txt", "keep-dir"} {
		_, err := os.Stat(filepath.Join(dir, name))
		assert.NoError(t, err)
	}
	assert.NoError(t, CleanupImageURLFiles(now+imageURLLifetime+1))
}

func TestImageURLMultiImageFailureRollsBackCreatedFiles(t *testing.T) {
	t.Setenv("DRAWING_STORAGE_DIR", t.TempDir())
	encoded := base64.StdEncoding.EncodeToString(imageURLTestPNG(t))
	body, err := common.Marshal(map[string]any{"data": []map[string]any{{"b64_json": encoded}, {"b64_json": "invalid"}}})
	require.NoError(t, err)
	result, err := FormatImageURLResponse(context.Background(), body, "https://images.example")
	require.Error(t, err)
	assert.Nil(t, result)
	entries, err := os.ReadDir(imageURLStorageDir())
	require.NoError(t, err)
	assert.Empty(t, entries)
}

type imageURLCancelBetweenItemsContext struct {
	context.Context
	checks int
}

func (ctx *imageURLCancelBetweenItemsContext) Err() error {
	ctx.checks++
	if ctx.checks >= 2 {
		return context.Canceled
	}
	return nil
}

func TestImageURLCancellationRollsBackCreatedFiles(t *testing.T) {
	t.Setenv("DRAWING_STORAGE_DIR", t.TempDir())
	encoded := base64.StdEncoding.EncodeToString(imageURLTestPNG(t))
	body, err := common.Marshal(map[string]any{"data": []map[string]any{{"b64_json": encoded}, {"b64_json": encoded}}})
	require.NoError(t, err)
	ctx := &imageURLCancelBetweenItemsContext{Context: context.Background()}
	result, err := FormatImageURLResponse(ctx, body, "https://images.example")
	require.ErrorContains(t, err, "cancelled")
	assert.Nil(t, result)
	entries, err := os.ReadDir(imageURLStorageDir())
	require.NoError(t, err)
	assert.Empty(t, entries)
}

func TestImageURLRejectsUntrustedBaseAddressBeforeWriting(t *testing.T) {
	t.Setenv("DRAWING_STORAGE_DIR", t.TempDir())
	encoded := base64.StdEncoding.EncodeToString(imageURLTestPNG(t))
	body, err := common.Marshal(map[string]any{"data": []map[string]any{{"b64_json": encoded}}})
	require.NoError(t, err)
	for _, address := range []string{"", "/relative", "file:///tmp", "https://user:secret@example.com", "https://example.com?token=secret", "https://example.com#fragment"} {
		result, err := FormatImageURLResponse(context.Background(), body, address)
		assert.Error(t, err)
		assert.Nil(t, result)
		assert.NotContains(t, err.Error(), "secret")
	}
	_, err = os.Stat(imageURLStorageDir())
	assert.ErrorIs(t, err, os.ErrNotExist)
}
