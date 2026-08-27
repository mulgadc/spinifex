package awserrors_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"testing"

	"github.com/mulgadc/spinifex/spinifex/awserrors"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestIsTerminal_ByErrorCode(t *testing.T) {
	cases := []struct {
		name     string
		code     string
		terminal bool
		why      string
	}{
		{"malformed request", awserrors.ErrorInvalidParameterValue, true, "the same parameters fail the same way"},
		{"absent resource", awserrors.ErrorInvalidInstanceIDNotFound, true, "the caller named something that is not there"},
		{"refused", awserrors.ErrorUnauthorizedOperation, true, "authorisation does not change by retrying"},
		{"unimplemented", awserrors.ErrorNotImplemented, true, "no amount of retrying implements it"},
		{"dry run", awserrors.ErrorDryRunOperation, true, "a dry run reports the same outcome every time"},

		{"throttled", awserrors.ErrorThrottling, false, "a throttle is an instruction to try again"},
		{"request rate", awserrors.ErrorRequestLimitExceeded, false, "same, at 503"},
		{"server side", awserrors.ErrorServerInternal, false, "the server may be healthy on the next pass"},
		{"unavailable", awserrors.ErrorServiceUnavailable, false, "temporary by definition"},
		{"already exists", awserrors.ErrorAccountAlreadyExists, false, "409s cover both racing writers and settled conflicts, so retry is the safe reading"},
		{"model not ready", awserrors.ErrorModelNotReadyException, false, "424 here means not ready yet, not a failed dependency"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.terminal, awserrors.IsTerminal(errors.New(tc.code)), tc.why)
		})
	}
}

// The predicate has to survive the wrapping every real call site applies, or it
// only works in tests.
func TestIsTerminal_ThroughWrapping(t *testing.T) {
	terminal := awserrors.Errorf(awserrors.ErrorInvalidParameterValue, "validate subnet %q", "subnet-1")
	assert.True(t, awserrors.IsTerminal(fmt.Errorf("reconcile eni-1: %w", terminal)))

	transient := awserrors.Errorf(awserrors.ErrorServiceUnavailable, "read state")
	assert.False(t, awserrors.IsTerminal(fmt.Errorf("reconcile eni-1: %w", transient)))

	joined := errors.Join(errors.New("unrelated"), terminal)
	assert.True(t, awserrors.IsTerminal(joined), "a joined tree is walked like a wrapped one")
}

func TestIsTerminal_Context(t *testing.T) {
	assert.True(t, awserrors.IsTerminal(context.Canceled),
		"nothing under a cancelled context runs again")
	assert.False(t, awserrors.IsTerminal(context.DeadlineExceeded),
		"a deadline may be met on a later attempt")
	assert.False(t, awserrors.IsTerminal(fmt.Errorf("load intent: %w", context.DeadlineExceeded)))
}

// The default is the whole design: an error nobody classified is retried, not
// dropped. Storage and NATS failures reach this path, since awserrors cannot
// import kvstore or nats without a cycle.
func TestIsTerminal_UnclassifiedIsTransient(t *testing.T) {
	assert.False(t, awserrors.IsTerminal(errors.New("nats: no responders available for request")))
	assert.False(t, awserrors.IsTerminal(errors.New("kvstore: key not found")))
	assert.False(t, awserrors.IsTerminal(errors.New("something nobody has seen before")))
}

func TestIsTerminal_Nil(t *testing.T) {
	assert.False(t, awserrors.IsTerminal(nil))
}

// Guards the table against ErrorLookup drift: a status listed as terminal must
// still be one no registered code reaches for a retryable reason.
func TestIsTerminal_TerminalStatusesAreClientFaults(t *testing.T) {
	for code, msg := range awserrors.ErrorLookup {
		if !awserrors.IsTerminal(errors.New(code)) {
			continue
		}
		// 501 is the one terminal 5xx: unimplemented is a property of this
		// server, not a condition that clears.
		if msg.HTTPCode == http.StatusNotImplemented {
			continue
		}
		require.Less(t, msg.HTTPCode, http.StatusInternalServerError,
			"code %q is terminal at status %d; a 5xx is the server's problem and may clear", code, msg.HTTPCode)
		require.NotEqual(t, http.StatusConflict, msg.HTTPCode,
			"code %q is terminal at 409; a conflict may clear", code)
		require.NotEqual(t, http.StatusTooManyRequests, msg.HTTPCode,
			"code %q is terminal at 429; a throttle is an instruction to retry", code)
	}
}
