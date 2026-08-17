package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/mulgadc/spinifex/spinifex/awserrors"
	"github.com/nats-io/nats.go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// These tests drive the real NATS transport rather than calling a handler
// function directly, because the defect they pin lived entirely in the response
// encoding: the handler already returned a well-worded error and only the wire
// payload dropped it, so a test calling the service function would have passed
// throughout. The neighbouring TestHandleNATSRequest_* cases assert Code only,
// which is why Message needs pinning separately.

// requestErrorEnvelope drives fn over NATS on subject and decodes the error
// envelope the handler wrote.
func requestErrorEnvelope(t *testing.T, subject string, fn func(context.Context, *testInput, string) (*testOutput, error)) map[string]any {
	t.Helper()

	nc, err := nats.Connect(sharedNATSURL)
	require.NoError(t, err)
	defer nc.Close()

	sub, err := nc.Subscribe(subject, asMsgHandler(handleNATSRequest(fn)))
	require.NoError(t, err)
	defer func() { _ = sub.Unsubscribe() }()

	reqData, err := json.Marshal(testInput{Name: "world"})
	require.NoError(t, err)
	reply, err := nc.Request(subject, reqData, 5*time.Second)
	require.NoError(t, err)

	var errResp map[string]any
	require.NoError(t, json.Unmarshal(reply.Data, &errResp))
	return errResp
}

// An unregistered error sanitizes to ServerInternal, but its reason must still
// reach the caller. This is the ACM force-renew refusal: without the message the
// CLI prints a bare "ServerInternal" and the operator has to read the daemon log
// to learn that only PRIVATE_CA certificates can be force-renewed.
func TestHandleNATSRequest_UncodedErrorCarriesMessage(t *testing.T) {
	const reason = "acm renewal: arn:aws:acm:ap-southeast-2:000000000001:certificate/abc is a AMAZON_ISSUED certificate; only PRIVATE_CA certificates can be force-renewed"

	errResp := requestErrorEnvelope(t, "test.err.msg.uncoded", func(context.Context, *testInput, string) (*testOutput, error) {
		return nil, errors.New(reason)
	})

	assert.Equal(t, awserrors.ErrorServerInternal, errResp["Code"])
	assert.Equal(t, reason, errResp["Message"],
		"the actionable reason must survive the transport, not only the daemon log")
}

// A wrapped coded error keeps the real code (so the gateway maps the right HTTP
// status) and carries the wrapping context alongside it.
func TestHandleNATSRequest_WrappedCodedErrorCarriesMessage(t *testing.T) {
	errResp := requestErrorEnvelope(t, "test.err.msg.wrapped", func(context.Context, *testInput, string) (*testOutput, error) {
		return nil, fmt.Errorf("vol-123 is attached to i-456: %w", errors.New(awserrors.ErrorVolumeInUse))
	})

	assert.Equal(t, awserrors.ErrorVolumeInUse, errResp["Code"])
	assert.Contains(t, errResp["Message"], "vol-123 is attached to i-456")
}

// A bare code carries no message. Handlers returning errors.New(awserrors.X) are
// the common case, and echoing the code back as its own message is noise.
func TestHandleNATSRequest_BareCodeOmitsMessage(t *testing.T) {
	errResp := requestErrorEnvelope(t, "test.err.msg.bare", func(context.Context, *testInput, string) (*testOutput, error) {
		return nil, errors.New(awserrors.ErrorInvalidParameterValue)
	})

	assert.Equal(t, awserrors.ErrorInvalidParameterValue, errResp["Code"])
	// The field is always present on the wire (no omitempty); left unset it
	// serializes as null rather than repeating the code back.
	assert.Nil(t, errResp["Message"])
}
