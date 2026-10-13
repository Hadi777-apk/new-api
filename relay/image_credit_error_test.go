package relay

import (
	"errors"
	"testing"

	"github.com/QuantumNous/new-api/relaykit/types"
	"github.com/stretchr/testify/assert"
)

func TestGrsaiImageCreditErrorNormalization(t *testing.T) {
	for _, tc := range []struct {
		name, base, message string
		status, expected    int
	}{
		{"global credits exhausted", "https://grsaiapi.com", "insufficient credits", 400, 402},
		{"China credits exhausted", "https://grsai.dakka.com.cn", "insufficient credits", 400, 402},
		{"case and whitespace", "https://GRSAIAPI.COM", " Insufficient Credits ", 400, 402},
		{"invalid size", "https://grsaiapi.com", "invalid size", 400, 400},
		{"embedded prompt text", "https://grsaiapi.com", "invalid prompt: insufficient credits", 400, 400},
		{"another provider", "https://api.goeasyapi.xyz", "insufficient credits", 400, 400},
		{"spoofed hostname", "https://grsaiapi.com.attacker.test", "insufficient credits", 400, 400},
		{"hostname in userinfo", "https://grsaiapi.com@attacker.test", "insufficient credits", 400, 400},
		{"timeout", "https://grsaiapi.com", "insufficient credits", 504, 504},
		{"success status", "https://grsaiapi.com", "insufficient credits", 200, 200},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := types.NewErrorWithStatusCode(errors.New(tc.message), types.ErrorCodeBadResponseStatusCode, tc.status)
			normalizeGrsaiImageCreditError(tc.base, err)
			assert.Equal(t, tc.expected, err.StatusCode)
			assert.Equal(t, tc.message, err.Error())
		})
	}
	assert.NotPanics(t, func() { normalizeGrsaiImageCreditError("https://grsaiapi.com", nil) })
}
