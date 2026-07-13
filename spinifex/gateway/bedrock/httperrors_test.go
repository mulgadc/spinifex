package gateway_bedrock

import (
	"net/http"
	"testing"

	"github.com/mulgadc/spinifex/spinifex/awserrors"
	"github.com/stretchr/testify/assert"
)

func TestMapUpstreamStatus(t *testing.T) {
	cases := []struct {
		name   string
		status int
		want   string
	}{
		{"401 unauthorized -> access denied", http.StatusUnauthorized, awserrors.ErrorAccessDeniedException},
		{"403 forbidden -> access denied", http.StatusForbidden, awserrors.ErrorAccessDeniedException},
		{"429 too many requests -> throttling", http.StatusTooManyRequests, awserrors.ErrorThrottlingException},
		{"400 bad request -> validation", http.StatusBadRequest, awserrors.ErrorValidationException},
		{"500 internal server error -> service unavailable", http.StatusInternalServerError, awserrors.ErrorServiceUnavailableException},
		{"503 service unavailable -> service unavailable", http.StatusServiceUnavailable, awserrors.ErrorServiceUnavailableException},
		{"418 teapot -> model error", http.StatusTeapot, awserrors.ErrorModelErrorException},
		{"other unmapped status -> model error", http.StatusConflict, awserrors.ErrorModelErrorException},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, mapUpstreamStatus(tc.status))
		})
	}
}
