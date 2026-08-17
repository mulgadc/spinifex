package gateway

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mulgadc/spinifex/spinifex/awserrors"
	handlers_iam "github.com/mulgadc/spinifex/spinifex/handlers/iam"
)

// A handler reached outside the middleware chain — a direct call from a test —
// has no audit record, so every accessor has to tolerate a nil receiver.
func TestRequestAuditNilSafe(t *testing.T) {
	audit := auditFrom(context.Background())
	require.Nil(t, audit)

	assert.NotPanics(t, func() {
		audit.setIdentity("AKIA", "123456789012", "ap-southeast-2", "ec2", "user")
		audit.setAction("ec2", "DescribeInstances")
		audit.setAuthError(awserrors.ErrorInvalidClientTokenId)
		audit.setAccessKeyID("AKIA")
		audit.annotate(context.Background())
	})
	assert.Nil(t, audit.fields())
}

// Empty values are omitted rather than logged blank, so an unauthenticated
// request does not carry a row of empty identity fields.
func TestRequestAuditOmitsEmptyFields(t *testing.T) {
	audit := &requestAudit{clientIP: "10.15.8.11"}
	assert.Equal(t, []any{"sourceIP", "10.15.8.11"}, audit.fields())

	audit.setIdentity("AKIAEXAMPLE", "123456789012", "ap-southeast-2", "ec2", "user")
	audit.setAction("ec2", "DescribeInstances")
	assert.Equal(t, []any{
		"sourceIP", "10.15.8.11",
		"accessKeyID", "AKIAEXAMPLE",
		"accountID", "123456789012",
		"region", "ap-southeast-2",
		"service", "ec2",
		"action", "DescribeInstances",
		"principalType", "user",
	}, audit.fields())
}

// The record may carry an access key id and an error code and nothing else that
// could hold a credential. This asserts the field set so a later addition of a
// session token or an Authorization header fails here first.
func TestRequestAuditCarriesNoSecret(t *testing.T) {
	audit := &requestAudit{}
	var keys []string
	for _, kv := range audit.pairs() {
		keys = append(keys, kv.key)
	}
	assert.Equal(t, []string{
		"sourceIP", "accessKeyID", "accountID", "region", "service", "action",
		"principalType", "authError",
	}, keys)
}

// auditRouter wires the audit middleware ahead of SigV4 auth the way
// SetupRoutes does, and hands back the record for the request just served.
func auditRouter(t *testing.T, keys map[string]*handlers_iam.AccessKey) (http.Handler, func() *requestAudit) {
	t.Helper()

	rl := NewAuthRateLimiter()
	t.Cleanup(rl.Stop)

	gw := &GatewayConfig{
		DisableLogging: true,
		Region:         testRegion,
		IAMService:     &mockIAMService{masterKey: testMasterKey, accessKeys: keys},
		RateLimiter:    rl,
	}

	var got *requestAudit
	r := chi.NewRouter()
	r.Use(requestAuditMiddleware)
	r.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			got = auditFrom(r.Context())
			next.ServeHTTP(w, r)
		})
	})
	r.Use(gw.SigV4AuthMiddleware())
	r.HandleFunc("/*", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("OK"))
	})

	return r, func() *requestAudit { return got }
}

// A request that never presented credentials is still attributable to where it
// came from, and carries the verdict it was answered with.
func TestRequestAuditRecordsUnauthenticatedRejection(t *testing.T) {
	handler, audit := auditRouter(t, nil)

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = "10.15.8.11:54321"
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	require.NotNil(t, audit())
	assert.Equal(t, "10.15.8.11", audit().clientIP)
	assert.Equal(t, awserrors.ErrorMissingAuthenticationToken, audit().authError)
	assert.Empty(t, audit().accessKeyID)
}

// signedRequest builds a request whose signature is wrong but whose envelope
// parses: the credential scope date must match X-Amz-Date or the request is
// rejected as malformed before the access key is ever looked at.
func signedRequest(remoteAddr string) *http.Request {
	now := time.Now().UTC()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = remoteAddr
	req.Header.Set("Authorization", "AWS4-HMAC-SHA256 Credential=AKIAINVALIDKEY000000/"+
		now.Format("20060102")+"/"+testRegion+"/ec2/aws4_request, "+
		"SignedHeaders=host;x-amz-date, Signature="+
		"0000000000000000000000000000000000000000000000000000000000000000")
	req.Header.Set("X-Amz-Date", now.Format("20060102T150405Z"))
	return req
}

// A rejected caller is identifiable: the key it presented is recorded even
// though authentication failed on it.
func TestRequestAuditRecordsRejectedAccessKey(t *testing.T) {
	handler, audit := auditRouter(t, map[string]*handlers_iam.AccessKey{})

	w := httptest.NewRecorder()
	handler.ServeHTTP(w, signedRequest("10.15.8.12:54321"))

	require.NotNil(t, audit())
	assert.Equal(t, "10.15.8.12", audit().clientIP)
	assert.Equal(t, "AKIAINVALIDKEY000000", audit().accessKeyID)
	assert.Equal(t, awserrors.ErrorInvalidClientTokenId, audit().authError)
}

// The lockout that answers 503 is the case that motivated this: without the
// record, the response carries no client address and no reason at all.
func TestRequestAuditRecordsRateLimitLockout(t *testing.T) {
	handler, audit := auditRouter(t, map[string]*handlers_iam.AccessKey{})

	send := func() int {
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, signedRequest("10.15.8.13:54321"))
		return w.Code
	}

	for range maxFailures {
		send()
	}
	assert.Equal(t, http.StatusServiceUnavailable, send())

	require.NotNil(t, audit())
	assert.Equal(t, "10.15.8.13", audit().clientIP)
	assert.Equal(t, awserrors.ErrorRequestLimitExceeded, audit().authError)
}
