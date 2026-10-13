package service

import (
	"bytes"
	"context"
	"encoding/base64"
	"image"
	"image/color"
	"image/png"
	"strings"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/relaykit/dto"
)

func patternPNGBase64(t testing.TB, width, height int) string {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, width, height))
	for i := 0; i < width*height; i++ {
		img.Set(i%width, i/width, color.RGBA{uint8(i * 7), uint8(i * 13), uint8(i * 29), 255})
	}
	var buffer bytes.Buffer
	if err := png.Encode(&buffer, img); err != nil {
		t.Fatal(err)
	}
	return base64.StdEncoding.EncodeToString(buffer.Bytes())
}

func TestDetachedImageJSONMatchesFullRoundTrip(t *testing.T) {
	one := patternPNGBase64(t, 96, 128)
	two := patternPNGBase64(t, 64, 64)
	escaped := strings.ReplaceAll(one, "/", `\/`)
	bodies := map[string]string{
		"single":            `{"created":1,"data":[{"b64_json":"` + one + `","revised_prompt":"a <b> & c"}],"usage":{"input_tokens":3,"output_tokens":7}}`,
		"two images":        `{"data":[{"b64_json":"` + one + `"},{"b64_json":"` + two + `","revised_prompt":"  ok"}],"quality":"high"}`,
		"escaped slashes":   `{"data":[{"b64_json":"` + escaped + `"}],"background":"opaque"}`,
		"whitespace":        "{\n  \"data\" : [ { \"b64_json\" : \"" + one + "\" } ] ,\n \"size\":\"9x9\"}",
		"null and empty":    `{"data":[{"b64_json":null,"url":"ftp://example.invalid/a.png"},{"b64_json":""}]}`,
		"not an image":      `{"data":[{"b64_json":"aGVsbG8gd29ybGQ="}]}`,
		"duplicate b64 key": `{"data":[{"b64_json":"` + two + `","b64_json":"` + one + `"}]}`,
		"duplicate data":    `{"data":[{"b64_json":"` + two + `"}],"data":[{"b64_json":"` + one + `"}]}`,
		"existing warnings": `{"data":[{"b64_json":"` + one + `"}],"output_warnings":[{"code":"upstream"}]}`,
		"not json":          `{"data":[{"b64_json":"` + one + `"`,
		"no data":           `{"error":{"message":"x"}}`,
	}
	requests := []*dto.ImageRequest{
		{Size: "96x128", Quality: "high"},
		{Size: "1024x1024", Quality: "low", N: common.GetPointer(uint(2))},
		{N: common.GetPointer(uint(1))},
		{Size: "auto"},
	}
	ctx := context.Background()
	for _, name := range []string{"single", "two images", "escaped slashes", "whitespace"} {
		stripped, blobs := detachImageBase64([]byte(bodies[name]))
		if blobs == nil || len(stripped) > 400 {
			t.Fatalf("%s: expected the image strings to be detached", name)
		}
	}
	for name, body := range bodies {
		t.Run(name, func(t *testing.T) {
			want := normalizeImageJSONResponse(ctx, []byte(body), nil)
			got := NormalizeImageJSONResponse(ctx, []byte(body))
			if !bytes.Equal(got, want) {
				t.Fatalf("normalize differs\n got: %.300s\nwant: %.300s", got, want)
			}
			for _, request := range requests {
				want := addImageOutputWarnings(want, request)
				got := AddImageOutputWarnings(got, request)
				if !bytes.Equal(got, want) {
					t.Fatalf("warnings differ for %+v\n got: %.300s\nwant: %.300s", *request, got, want)
				}
			}
		})
	}
}

func TestImageJSONWithoutBase64KeepsUsageAndErrors(t *testing.T) {
	body := []byte(`{"data":[{"b64_json":"` + patternPNGBase64(t, 32, 32) + `"}],"usage":{"input_tokens":5,"output_tokens":9},"timings":{"cache_n":4}}`)
	stripped := ImageJSONWithoutBase64(body)
	if len(stripped) > 300 {
		t.Fatalf("expected image string to be detached, got %d bytes", len(stripped))
	}
	var full, small dto.SimpleResponse
	if err := common.Unmarshal(body, &full); err != nil {
		t.Fatal(err)
	}
	if err := common.Unmarshal(stripped, &small); err != nil {
		t.Fatal(err)
	}
	if full.Usage.InputTokens != small.Usage.InputTokens || full.Usage.OutputTokens != small.Usage.OutputTokens {
		t.Fatalf("usage changed: %+v vs %+v", full.Usage, small.Usage)
	}
	invalid := []byte(`{"data":[`)
	if !bytes.Equal(ImageJSONWithoutBase64(invalid), invalid) {
		t.Fatal("invalid JSON must be returned unchanged so decoding still fails")
	}
}
