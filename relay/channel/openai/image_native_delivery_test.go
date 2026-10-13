package openai

import (
	"bytes"
	"encoding/base64"
	"image"
	"image/png"
	"net/http"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/relaykit/types"
	"github.com/QuantumNous/new-api/service"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestImageNativeDeliveryValidatesPublicAndPrivateResponses(t *testing.T) {
	var imageBytes bytes.Buffer
	require.NoError(t, png.Encode(&imageBytes, image.NewGray(image.Rect(0, 0, 2, 3))))
	encoded := base64.StdEncoding.EncodeToString(imageBytes.Bytes())
	for _, private := range []bool{false, true} {
		for _, test := range []struct {
			name, value string
			valid       bool
		}{
			{"native PNG", encoded, true},
			{"data URL uses actual format", "data:image/jpeg;base64," + encoded, true},
			{"invalid Base64", "not-base64!", false},
			{"truncated PNG", base64.StdEncoding.EncodeToString(imageBytes.Bytes()[:33]), false},
		} {
			label := "public/" + test.name
			if private {
				label = "private/" + test.name
			}
			t.Run(label, func(t *testing.T) {
				body, err := common.Marshal(map[string]any{"data": []map[string]any{{"b64_json": test.value, "url": "http://127.0.0.1:1/unused?secret=hidden"}}, "usage": map[string]int{"input_tokens": 7, "output_tokens": 3}})
				require.NoError(t, err)
				c, recorder, response, info := newImageTestContext(t, string(body), "application/json", false)
				c.Set(service.DrawingResponseSpoolContextKey, private)
				usage, apiErr := OpenaiImageHandler(c, info, response)
				if !test.valid {
					require.NotNil(t, apiErr)
					assert.Nil(t, usage)
					assert.Equal(t, http.StatusFailedDependency, apiErr.StatusCode)
					assert.True(t, types.IsSkipRetryError(apiErr))
					assert.Equal(t, "false", recorder.Header().Get("x-should-retry"))
					assert.False(t, c.Writer.Written())
					assert.Empty(t, recorder.Body.String())
					assert.NotContains(t, apiErr.Error(), "hidden")
					return
				}
				require.Nil(t, apiErr)
				require.NotNil(t, usage)
				assert.Equal(t, encoded, gjson.GetBytes(recorder.Body.Bytes(), "data.0.b64_json").String())
				assert.Equal(t, "2x3", gjson.GetBytes(recorder.Body.Bytes(), "size").String())
				assert.Equal(t, "png", gjson.GetBytes(recorder.Body.Bytes(), "output_format").String())
				assert.EqualValues(t, 7, usage.PromptTokens)
				assert.EqualValues(t, 3, usage.CompletionTokens)
			})
		}
	}
}
