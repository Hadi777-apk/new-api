package service

import (
	"bytes"
	"context"
	"encoding/base64"
	"image"
	"image/png"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestImageOutputMismatchReportsFactsWithoutChangingImage(t *testing.T) {
	var pngBytes bytes.Buffer
	require.NoError(t, png.Encode(&pngBytes, image.NewRGBA(image.Rect(0, 0, 4, 4))))
	encoded := base64.StdEncoding.EncodeToString(pngBytes.Bytes())
	body, err := common.Marshal(map[string]any{"data": []map[string]any{{"b64_json": encoded}}, "quality": "low", "output_format": "jpeg", "usage": map[string]int{"total_tokens": 20}})
	require.NoError(t, err)
	out := AddImageOutputWarnings(NormalizeImageJSONResponse(context.Background(), body), &dto.ImageRequest{Size: "960x1280", Quality: "high"})
	assert.Equal(t, encoded, gjson.GetBytes(out, "data.0.b64_json").String())
	assert.Equal(t, "4x4", gjson.GetBytes(out, "size").String())
	assert.Equal(t, "png", gjson.GetBytes(out, "output_format").String())
	assert.Equal(t, "960x1280", gjson.GetBytes(out, "requested_size").String())
	assert.False(t, gjson.GetBytes(out, "data.0.size_matches_request").Bool())
	assert.False(t, gjson.GetBytes(out, "data.0.aspect_ratio_matches_request").Bool())
	assert.Equal(t, "low", gjson.GetBytes(out, "quality").String())
	assert.Equal(t, "high", gjson.GetBytes(out, "requested_quality").String())
	assert.False(t, gjson.GetBytes(out, "quality_report_matches_request").Bool())
	assert.EqualValues(t, 20, gjson.GetBytes(out, "usage.total_tokens").Int())
	assert.Contains(t, string(out), "image_aspect_ratio_mismatch")
	assert.Contains(t, string(out), "image_quality_report_mismatch")
}

func TestImageOutputSameRatioDoesNotPretendExactSizeOrKnownQuality(t *testing.T) {
	body := []byte(`{"data":[{"width":1086,"height":1448,"size":"1086x1448","b64_json":"unchanged"}],"quality":null}`)
	out := AddImageOutputWarnings(body, &dto.ImageRequest{Size: "960x1280", Quality: "low"})
	assert.False(t, gjson.GetBytes(out, "data.0.size_matches_request").Bool())
	assert.True(t, gjson.GetBytes(out, "data.0.aspect_ratio_matches_request").Bool())
	assert.Equal(t, "null", gjson.GetBytes(out, "quality_report_matches_request").Raw)
	assert.NotContains(t, string(out), "image_quality_report_mismatch")
}

func TestImageOutputCountMismatchWithAutoSettingsPreservesData(t *testing.T) {
	for _, tc := range []struct {
		name, settings, data string
		requested            uint
		returned             int64
	}{
		{"extra images without settings", "", `[{"b64_json":"first","vendor_image":{"original":true}},{"b64_json":"second"},{"url":"https://example.test/third.png"}]`, 1, 3},
		{"fewer images with auto settings", "auto", `[{"b64_json":"first"}]`, 2, 1},
		{"empty output", "auto", `[]`, 1, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := []byte(`{"data":` + tc.data + `,"vendor":{"job":"retained"},"usage":{"total_tokens":20}}`)
			out := AddImageOutputWarnings(body, &dto.ImageRequest{Size: tc.settings, Quality: tc.settings, N: common.GetPointer(tc.requested)})
			assert.JSONEq(t, gjson.GetBytes(body, "data").Raw, gjson.GetBytes(out, "data").Raw)
			assert.JSONEq(t, gjson.GetBytes(body, "vendor").Raw, gjson.GetBytes(out, "vendor").Raw)
			assert.JSONEq(t, gjson.GetBytes(body, "usage").Raw, gjson.GetBytes(out, "usage").Raw)
			assert.EqualValues(t, tc.requested, gjson.GetBytes(out, "requested_n").Int())
			assert.Equal(t, tc.returned, gjson.GetBytes(out, "returned_n").Int())
			assert.EqualValues(t, 1, gjson.GetBytes(out, "output_warnings.#").Int())
			assert.Equal(t, "image_count_mismatch", gjson.GetBytes(out, "output_warnings.0.code").String())
			assert.EqualValues(t, tc.requested, gjson.GetBytes(out, "output_warnings.0.requested").Int())
			assert.Equal(t, tc.returned, gjson.GetBytes(out, "output_warnings.0.actual").Int())
		})
	}
}

func TestImageOutputCountMismatchMergesExistingSizeAndQualityWarnings(t *testing.T) {
	body := []byte(`{"data":[{"width":800,"height":800,"size":"800x800","b64_json":"unchanged"}],"quality":"low","output_warnings":[{"code":"vendor_warning","vendor":{"detail":"retained","id":9007199254740993}}],"usage":{"total_tokens":20}}`)
	out := AddImageOutputWarnings(body, &dto.ImageRequest{Size: "960x1280", Quality: "high", N: common.GetPointer(uint(2))})
	assert.Equal(t, "unchanged", gjson.GetBytes(out, "data.0.b64_json").String())
	assert.Equal(t, "800x800", gjson.GetBytes(out, "data.0.size").String())
	assert.EqualValues(t, 20, gjson.GetBytes(out, "usage.total_tokens").Int())
	assert.Equal(t, "960x1280", gjson.GetBytes(out, "requested_size").String())
	assert.Equal(t, "high", gjson.GetBytes(out, "requested_quality").String())
	assert.Equal(t, "retained", gjson.GetBytes(out, `output_warnings.#(code=="vendor_warning").vendor.detail`).String())
	assert.Equal(t, "9007199254740993", gjson.GetBytes(out, `output_warnings.#(code=="vendor_warning").vendor.id`).Raw)
	for _, code := range []string{"image_count_mismatch", "image_size_mismatch", "image_aspect_ratio_mismatch", "image_quality_report_mismatch"} {
		assert.True(t, gjson.GetBytes(out, `output_warnings.#(code=="`+code+`")`).Exists(), code)
	}
}

func TestImageOutputMatchingOrInvalidCountKeepsOriginalResponseBytes(t *testing.T) {
	body := []byte("{ \"data\" : [{\"b64_json\":\"unchanged\"}], \"vendor\":true }\n")
	for _, settings := range []string{"", "auto"} {
		for _, n := range []*uint{nil, common.GetPointer(uint(0)), common.GetPointer(uint(1)), common.GetPointer(uint(dto.MaxImageN + 1))} {
			out := AddImageOutputWarnings(body, &dto.ImageRequest{Size: settings, Quality: settings, N: n})
			assert.Equal(t, body, out)
		}
	}
}

func TestImageOutputCountDoesNotRewriteInvalidEnvelope(t *testing.T) {
	for _, body := range []string{`not-json`, `{}`, `{"data":null}`, `{"data":{"b64_json":"unchanged"}}`} {
		out := AddImageOutputWarnings([]byte(body), &dto.ImageRequest{N: common.GetPointer(uint(1))})
		assert.Equal(t, body, string(out))
	}
}
