package audit

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	awserrors "github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
)

// A handler reached outside the middleware chain — a direct call from a test —
// has no audit record, so every accessor has to tolerate a nil receiver.
func TestRequestAuditNilSafe(t *testing.T) {
	audit := From(context.Background())
	require.Nil(t, audit)

	assert.NotPanics(t, func() {
		audit.SetIdentity("AKIA", "123456789012", "ap-southeast-2", "ec2", "user")
		audit.SetAction("ec2", "DescribeInstances")
		audit.SetAuthError(awserrors.ErrorInvalidClientTokenId)
		audit.SetAccessKeyID("AKIA")
		audit.annotate(context.Background())
	})
	assert.Nil(t, audit.Fields())
}

// Empty values are omitted rather than logged blank, so an unauthenticated
// request does not carry a row of empty identity fields.
func TestRequestAuditOmitsEmptyFields(t *testing.T) {
	audit := &Record{clientIP: "10.15.8.11"}
	assert.Equal(t, []any{"sourceIP", "10.15.8.11"}, audit.Fields())

	audit.SetIdentity("AKIAEXAMPLE", "123456789012", "ap-southeast-2", "ec2", "user")
	audit.SetAction("ec2", "DescribeInstances")
	assert.Equal(t, []any{
		"sourceIP", "10.15.8.11",
		"accessKeyID", "AKIAEXAMPLE",
		"accountID", "123456789012",
		"region", "ap-southeast-2",
		"service", "ec2",
		"action", "DescribeInstances",
		"principalType", "user",
	}, audit.Fields())
}

// The record may carry an access key id and an error code and nothing else that
// could hold a credential. This asserts the field set so a later addition of a
// session token or an Authorization header fails here first.
func TestRequestAuditCarriesNoSecret(t *testing.T) {
	audit := &Record{}
	var keys []string
	for _, kv := range audit.pairs() {
		keys = append(keys, kv.key)
	}
	assert.Equal(t, []string{
		"sourceIP", "accessKeyID", "accountID", "region", "service", "action",
		"principalType", "authError", "requestID", "awsRequestID",
	}, keys)
}

// slog renders a bare time.Duration as unitless nanoseconds, so the access line
// carries duration_ms instead.
func TestLogRequestDurationInMilliseconds(t *testing.T) {
	buf := &bytes.Buffer{}
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelInfo})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	audit := &Record{clientIP: "198.51.100.7", requestID: "abc123"}
	req = req.WithContext(context.WithValue(req.Context(), ctxAudit, audit))

	logRequest(req, http.StatusOK, 1500*time.Millisecond)

	out := buf.String()
	assert.Contains(t, out, "duration_ms=1500")
	assert.NotContains(t, out, "duration=")
	assert.Contains(t, out, "sourceIP=198.51.100.7")
	assert.Contains(t, out, "requestID=abc123")
}
