package dispatch

import (
	"bytes"
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/mulgadc/bluebottle/pkg/sigv4"
	awserrors "github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/mulgadc/spinifex/spinifex/ingress/aws/query"
)

// ReadBoundedBody reads the body under the SigV4 payload cap, so a body read ahead
// of the policy gate can neither bypass it nor exhaust memory. A read failure is
// InvalidParameterValue; an oversized body is RequestEntityTooLarge.
func ReadBoundedBody(r *http.Request) ([]byte, error) {
	body, err := io.ReadAll(io.LimitReader(r.Body, sigv4.MaxPayloadLen+1))
	if err != nil {
		return nil, errors.New(awserrors.ErrorInvalidParameterValue)
	}
	if int64(len(body)) > sigv4.MaxPayloadLen {
		return nil, errors.New(awserrors.ErrorRequestEntityTooLarge)
	}
	return body, nil
}

// ReadArgs parses a query-protocol body, capped at the SigV4 payload limit, and
// rewinds r.Body so a later reader sees the same bytes.
func ReadArgs(r *http.Request) (map[string]string, error) {
	body, err := io.ReadAll(io.LimitReader(r.Body, sigv4.MaxPayloadLen+1))
	if err != nil {
		return nil, err
	}
	r.Body = io.NopCloser(bytes.NewReader(body))
	if int64(len(body)) > sigv4.MaxPayloadLen {
		return nil, errors.New(awserrors.ErrorRequestEntityTooLarge)
	}
	return query.ParseArgs(string(body))
}

// ResolveQueryAction is the Query Action parser telemetry and dispatch share. It
// leaves the body readable and answers "" when it is unreadable, oversized or malformed.
func ResolveQueryAction(r *http.Request) string {
	args, err := ReadArgs(r)
	if err != nil {
		return ""
	}
	return args["Action"]
}

// ActionFromTarget returns the operation named by a JSON-protocol X-Amz-Target
// header ("Prefix.Operation"); a target without a dot is returned unchanged.
func ActionFromTarget(target string) string {
	if _, after, ok := strings.CutLast(target, "."); ok {
		return after
	}
	return target
}

// ResolveTargetAction returns the operation named by the request's X-Amz-Target header.
func ResolveTargetAction(r *http.Request) string {
	return ActionFromTarget(r.Header.Get("X-Amz-Target"))
}
