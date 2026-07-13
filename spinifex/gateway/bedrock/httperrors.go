package gateway_bedrock

import (
	"net/http"

	"github.com/mulgadc/spinifex/spinifex/awserrors"
)

// mapUpstreamStatus maps an upstream provider HTTP status to a bedrock
// awserrors code, shared by the Anthropic and vLLM adapters.
func mapUpstreamStatus(status int) string {
	switch {
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		return awserrors.ErrorAccessDeniedException
	case status == http.StatusTooManyRequests:
		return awserrors.ErrorThrottlingException
	case status == http.StatusBadRequest:
		return awserrors.ErrorValidationException
	case status >= 500:
		return awserrors.ErrorServiceUnavailableException
	default:
		return awserrors.ErrorModelErrorException
	}
}
