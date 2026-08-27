package awserrors

import (
	"context"
	"errors"
	"net/http"
)

// terminalStatus lists the HTTP statuses whose AWS codes describe a request that
// cannot succeed if repeated unchanged: malformed, refused, absent or
// unimplemented. Everything absent from this map is transient.
//
// 409 and 429 are deliberately excluded — a conflict or a throttle is the
// caller being told to try again. So is 424, which is bedrock's
// ModelNotReadyException rather than a failed dependency in the usual sense.
var terminalStatus = map[int]bool{
	http.StatusBadRequest:            true,
	http.StatusForbidden:             true,
	http.StatusNotFound:              true,
	http.StatusMethodNotAllowed:      true,
	http.StatusPreconditionFailed:    true,
	http.StatusRequestEntityTooLarge: true,
	http.StatusNotImplemented:        true,
}

// IsTerminal reports whether err will fail again if the identical call is
// repeated. It is the predicate a reconcile loop needs to choose between
// requeueing work and dropping it.
//
// An error it cannot classify is transient, so unrecognised failures are
// retried rather than discarded: a bounded retry costs time, whereas dropping
// work that would have succeeded loses it silently and without a signal.
//
// Classification comes from the AWS code's registered HTTP status, so it tracks
// ErrorLookup rather than a second, drifting judgement about the same codes.
func IsTerminal(err error) bool {
	if err == nil {
		return false
	}

	// A deadline may be met on a later attempt; a cancellation will not, because
	// nothing under that context runs again.
	if errors.Is(err, context.Canceled) {
		return true
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return false
	}

	code, ok := ResolveErrorCode(err)
	if !ok {
		return false
	}
	return terminalStatus[ErrorLookup[code].HTTPCode]
}
