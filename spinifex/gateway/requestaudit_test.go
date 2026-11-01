package gateway

import (
	"fmt"
	"github.com/mulgadc/spinifex/internal/testkit/gwfix"
	authlimit "github.com/mulgadc/spinifex/spinifex/ingress/aws/ratelimit"
	"github.com/mulgadc/spinifex/spinifex/runtime/roles/awsgw/credverify"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	iamdomain "github.com/mulgadc/spinifex/spinifex/domains/iam"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/mulgadc/spinifex/spinifex/ingress/aws/audit"
)

// auditRouter wires the audit middleware ahead of SigV4 auth the way
// SetupRoutes does, and hands back the record for the request just served.
func auditRouter(t *testing.T, keys map[string]*iamdomain.AccessKey) (http.Handler, func() *audit.Record) {
	t.Helper()

	rl := authlimit.NewAuthRateLimiter()
	t.Cleanup(rl.Stop)

	gw := gwfix.WithDeps(&GatewayConfig{
		DisableLogging: true,
		Region:         gwfix.Region,
		Credentials:    credverify.New(&gwfix.IAM{MasterKey: gwfix.MasterKey, AccessKeys: keys}, nil),
		RateLimiter:    rl,
	}, gwfix.Deps{IAM: &gwfix.IAM{MasterKey: gwfix.MasterKey, AccessKeys: keys}})
	withEC2(gw)

	var got *audit.Record
	r := chi.NewRouter()
	r.Use(audit.Middleware)
	r.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			got = audit.From(r.Context())
			next.ServeHTTP(w, r)
		})
	})
	r.Use(gw.SigV4AuthMiddleware())
	r.HandleFunc("/*", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("OK"))
	})

	return r, func() *audit.Record { return got }
}

// signedRequest builds a request whose signature is wrong but whose envelope
// parses: the credential scope date must match X-Amz-Date or the request is
// rejected as malformed before the access key is ever looked at.
func signedRequest(remoteAddr string) *http.Request {
	return signedRequestForKey(remoteAddr, "AKIAINVALIDKEY000000")
}

// signedRequestForKey is signedRequest with the presented key id chosen by the
// caller, so a test can send distinct attempts rather than one repeated.
func signedRequestForKey(remoteAddr, accessKeyID string) *http.Request {
	now := time.Now().UTC()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = remoteAddr
	req.Header.Set("Authorization", "AWS4-HMAC-SHA256 Credential="+accessKeyID+"/"+
		now.Format("20060102")+"/"+gwfix.Region+"/ec2/aws4_request, "+
		"SignedHeaders=host;x-amz-date, Signature="+
		"0000000000000000000000000000000000000000000000000000000000000000")
	req.Header.Set("X-Amz-Date", now.Format("20060102T150405Z"))
	return req
}

// The lockout that answers 503 is the case that motivated this: without the
// record, the response carries no client address and no reason at all.
func TestRequestAuditRecordsRateLimitLockout(t *testing.T) {
	handler, record := auditRouter(t, map[string]*iamdomain.AccessKey{})

	// Distinct key ids: one key id repeated is one fault and never locks out.
	send := func(accessKeyID string) int {
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, signedRequestForKey("10.15.8.13:54321", accessKeyID))
		return w.Code
	}

	for i := range authlimit.MaxFailures {
		send(fmt.Sprintf("AKIAGUESS%011d", i))
	}
	assert.Equal(t, http.StatusServiceUnavailable, send("AKIAINVALIDKEY000000"))

	require.NotNil(t, record())
	assert.Equal(t, "10.15.8.13", auditField(record(), "sourceIP"))
	assert.Equal(t, awserrors.ErrorRequestLimitExceeded, auditField(record(), "authError"))
}

// sourceIP must not be something a direct caller can choose. X-Real-IP is the
// edge's word only on a loopback connection; from anywhere else both it and
// X-Forwarded-For are ignored.
func TestRequestAuditClientIPIgnoresSpoofedHeadersFromNonLoopback(t *testing.T) {
	handler, record := auditRouter(t, nil)

	for _, remote := range []string{"203.0.113.9:5555", "10.2.0.3:5555", "[2001:db8::9]:5555"} {
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		req.RemoteAddr = remote
		req.Header.Set("X-Real-IP", "127.0.0.1")
		req.Header.Set("X-Forwarded-For", "198.51.100.1")
		handler.ServeHTTP(httptest.NewRecorder(), req)

		require.NotNil(t, record())
		host, _, err := net.SplitHostPort(remote)
		require.NoError(t, err)
		assert.Equal(t, host, auditField(record(), "sourceIP"), "spoofed headers from %s must not be logged as the source", remote)
	}
}

// Through the edge (or the console proxy) the connection is loopback, and the
// edge's X-Real-IP is the real client.
func TestRequestAuditClientIPTrustsRealIPFromLoopback(t *testing.T) {
	handler, record := auditRouter(t, nil)

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = "127.0.0.1:40000"
	req.Header.Set("X-Real-IP", "198.51.100.7")
	req.Header.Set("X-Forwarded-For", "203.0.113.1")
	handler.ServeHTTP(httptest.NewRecorder(), req)

	require.NotNil(t, record())
	assert.Equal(t, "198.51.100.7", auditField(record(), "sourceIP"))
}

// The edge's request ID joins an awsgw line to its access-log line; a value that
// does not look like an ID is dropped rather than written into the log.
func TestRequestAuditRecordsRequestID(t *testing.T) {
	handler, record := auditRouter(t, nil)

	cases := map[string]string{
		"4f1c2b7e9a0d4e6f8b3c5a7d9e1f2a3b": "4f1c2b7e9a0d4e6f8b3c5a7d9e1f2a3b",
		"":                                 "",
		"has space":                        "",
		strings.Repeat("a", 129):           "",
	}
	for header, want := range cases {
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		req.RemoteAddr = "127.0.0.1:40000"
		if header != "" {
			req.Header.Set("X-Request-ID", header)
		}
		handler.ServeHTTP(httptest.NewRecorder(), req)

		require.NotNil(t, record())
		assert.Equal(t, want, auditField(record(), "requestID"), "X-Request-ID %q", header)
	}
}

// auditField returns one value from the record's log fields, or "" when the
// record omits it as empty.
func auditField(rec *audit.Record, key string) string {
	f := rec.Fields()
	for i := 0; i+1 < len(f); i += 2 {
		if f[i] == key {
			v, _ := f[i+1].(string)
			return v
		}
	}
	return ""
}

// A rejected caller is identifiable: the key it presented is recorded even
// though authentication failed on it.
func TestRequestAuditRecordsRejectedAccessKey(t *testing.T) {
	handler, record := auditRouter(t, map[string]*iamdomain.AccessKey{})

	w := httptest.NewRecorder()
	handler.ServeHTTP(w, signedRequest("10.15.8.12:54321"))

	require.NotNil(t, record())
	assert.Equal(t, "10.15.8.12", auditField(record(), "sourceIP"))
	assert.Equal(t, "AKIAINVALIDKEY000000", auditField(record(), "accessKeyID"))
	assert.Equal(t, awserrors.ErrorInvalidClientTokenId, auditField(record(), "authError"))
}

// A request that never presented credentials is still attributable to where it
// came from, and carries the verdict it was answered with.
func TestRequestAuditRecordsUnauthenticatedRejection(t *testing.T) {
	handler, record := auditRouter(t, nil)

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = "10.15.8.11:54321"
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	require.NotNil(t, record())
	assert.Equal(t, "10.15.8.11", auditField(record(), "sourceIP"))
	assert.Equal(t, awserrors.ErrorMissingAuthenticationToken, auditField(record(), "authError"))
	assert.Empty(t, auditField(record(), "accessKeyID"))
}
