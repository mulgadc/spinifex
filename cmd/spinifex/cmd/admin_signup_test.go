package cmd_test

import (
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/mulgadc/spinifex/cmd/spinifex/cmd"
	"github.com/mulgadc/spinifex/spinifex/config"
	"github.com/mulgadc/spinifex/spinifex/gateway"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const auditAccountID = "000000000001"

// STS gates AssumeRole on the trust policy alone, so an account-wide trust
// policy is assumable by every principal in the account — including the
// single-action signup credential.
func TestTrustsWholeAccount(t *testing.T) {
	tests := []struct {
		name     string
		document string
		want     bool
	}{
		{
			name:     "root ARN of the account",
			document: `{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Principal":{"AWS":"arn:aws:iam::000000000001:root"},"Action":"sts:AssumeRole"}]}`,
			want:     true,
		},
		{
			name:     "bare account ID",
			document: `{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Principal":{"AWS":"000000000001"},"Action":"sts:AssumeRole"}]}`,
			want:     true,
		},
		{
			name:     "wildcard",
			document: `{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Principal":{"AWS":"*"},"Action":"sts:AssumeRole"}]}`,
			want:     true,
		},
		{
			name:     "account-wide among several entries",
			document: `{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Principal":{"AWS":["arn:aws:iam::000000000001:user/alice","000000000001"]},"Action":"sts:AssumeRole"}]}`,
			want:     true,
		},
		{
			name:     "specific user",
			document: `{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Principal":{"AWS":"arn:aws:iam::000000000001:user/alice"},"Action":"sts:AssumeRole"}]}`,
			want:     false,
		},
		{
			name:     "another account",
			document: `{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Principal":{"AWS":"arn:aws:iam::123456789012:root"},"Action":"sts:AssumeRole"}]}`,
			want:     false,
		},
		{
			name:     "service principal",
			document: `{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Principal":{"Service":"ec2.amazonaws.com"},"Action":"sts:AssumeRole"}]}`,
			want:     false,
		},
		{
			name:     "deny statement is not a grant",
			document: `{"Version":"2012-10-17","Statement":[{"Effect":"Deny","Principal":{"AWS":"*"},"Action":"sts:AssumeRole"}]}`,
			want:     false,
		},
		{
			name:     "unparseable document is reported rather than passed over",
			document: `not json`,
			want:     true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, cmd.TrustsWholeAccount(tc.document, auditAccountID))
		})
	}
}

func TestNewClientTokenIsAccepted(t *testing.T) {
	valid := regexp.MustCompile(`^[A-Za-z0-9_-]{32,128}$`)

	first := cmd.NewClientToken()
	second := cmd.NewClientToken()

	assert.Regexp(t, valid, first)
	assert.NotEqual(t, first, second)
}

func TestLocalGatewayEndpoint(t *testing.T) {
	tests := []struct {
		name string
		host string
		want string
	}{
		{"explicit host", "10.0.0.1:9999", "https://10.0.0.1:9999"},
		{"wildcard bind is not dialable", "0.0.0.0:9999", "https://localhost:9999"},
		{"empty host", ":9999", "https://localhost:9999"},
		{"no port", "10.0.0.1", ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			node := config.Config{AWSGW: config.AWSGWConfig{Host: tc.host}}
			assert.Equal(t, tc.want, cmd.LocalGatewayEndpoint(node))
		})
	}
}

// An unreadable or empty CA bundle must be an error, never a silent fallback
// to an unverified connection.
func TestAdminHTTPClientRejectsBadCABundle(t *testing.T) {
	_, err := cmd.AdminHTTPClient(filepath.Join(t.TempDir(), "absent.pem"))
	assert.Error(t, err)

	empty := filepath.Join(t.TempDir(), "empty.pem")
	require.NoError(t, os.WriteFile(empty, []byte("not a certificate"), 0o600))
	_, err = cmd.AdminHTTPClient(empty)
	assert.Error(t, err)
}

// writeServerCA writes a test server's certificate where adminHTTPClient can
// trust it, standing in for a cluster's self-signed CA.
func writeServerCA(t *testing.T, srv *httptest.Server) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "ca.pem")
	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw})
	require.NoError(t, os.WriteFile(path, pemBytes, 0o600))
	return path
}

// newAdminTestServer serves one canned response over TLS and records the
// request the client actually sent.
func newAdminTestServer(t *testing.T, status int, body string, got *http.Request) *httptest.Server {
	t.Helper()
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*got = *r
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv
}

const createAccountOK = `{"accountId":"000000000042","accountName":"ben@example.com","adminUser":"admin",` +
	`"accessKeyId":"AKIATEST","secretAccessKey":"secret","defaultVpcId":"vpc-1"}`

// The request must be SigV4-signed for service spinifex at the configured
// region, and must reach /admin/CreateAccount as a POST.
func TestCreateAccountRemoteSignsAndPosts(t *testing.T) {
	t.Setenv("AWS_ACCESS_KEY_ID", "AKIAEXAMPLE")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY")

	var got http.Request
	srv := newAdminTestServer(t, http.StatusOK, createAccountOK, &got)

	out, err := cmd.CreateAccountRemote(t.Context(),
		cmd.AdminTarget(srv.URL, "us-west-1", writeServerCA(t, srv)),
		gateway.CreateAccountRequest{Name: "ben@example.com", ClientToken: strings.Repeat("a", 32)})
	require.NoError(t, err)

	assert.Equal(t, "000000000042", out.AccountID)
	assert.Equal(t, "secret", out.SecretAccessKey)

	assert.Equal(t, http.MethodPost, got.Method)
	assert.Equal(t, "/admin/CreateAccount", got.URL.Path)
	assert.Equal(t, "application/json", got.Header.Get("Content-Type"))
	assert.NotEmpty(t, got.Header.Get("X-Amz-Content-Sha256"))
	assert.Contains(t, got.Header.Get("Authorization"), "AWS4-HMAC-SHA256")
	assert.Contains(t, got.Header.Get("Authorization"), "/us-west-1/spinifex/aws4_request")
}

// A trailing slash on the endpoint must not produce a double slash in the path,
// which SigV4 would sign and the gateway would not route.
func TestCreateAccountRemoteTrimsEndpointSlash(t *testing.T) {
	t.Setenv("AWS_ACCESS_KEY_ID", "AKIAEXAMPLE")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY")

	var got http.Request
	srv := newAdminTestServer(t, http.StatusOK, createAccountOK, &got)

	_, err := cmd.CreateAccountRemote(t.Context(),
		cmd.AdminTarget(srv.URL+"/", "us-west-1", writeServerCA(t, srv)),
		gateway.CreateAccountRequest{Name: "ben@example.com", ClientToken: strings.Repeat("a", 32)})
	require.NoError(t, err)
	assert.Equal(t, "/admin/CreateAccount", got.URL.Path)
}

func TestCreateAccountRemoteSurfacesGatewayError(t *testing.T) {
	t.Setenv("AWS_ACCESS_KEY_ID", "AKIAEXAMPLE")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY")

	var got http.Request
	srv := newAdminTestServer(t, http.StatusConflict,
		`{"error":{"code":"AccountAlreadyExists","message":"taken"},"requestId":"req-1"}`, &got)

	_, err := cmd.CreateAccountRemote(t.Context(),
		cmd.AdminTarget(srv.URL, "us-west-1", writeServerCA(t, srv)),
		gateway.CreateAccountRequest{Name: "ben@example.com", ClientToken: strings.Repeat("a", 32)})

	require.Error(t, err)
	assert.Contains(t, err.Error(), "AccountAlreadyExists")
	assert.Contains(t, err.Error(), "req-1")
	assert.False(t, cmd.RetryableAdminError(err), "a taken name is not fixed by retrying")
}

// An untrusted certificate must fail rather than fall back to no verification.
func TestCreateAccountRemoteRefusesUntrustedServer(t *testing.T) {
	t.Setenv("AWS_ACCESS_KEY_ID", "AKIAEXAMPLE")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY")

	var got http.Request
	srv := newAdminTestServer(t, http.StatusOK, createAccountOK, &got)

	_, err := cmd.CreateAccountRemote(t.Context(),
		cmd.AdminTarget(srv.URL, "us-west-1", ""),
		gateway.CreateAccountRequest{Name: "ben@example.com", ClientToken: strings.Repeat("a", 32)})
	assert.Error(t, err)
}

// Only a lost or in-flight result is worth retrying. Suggesting it for the
// rest invites a fresh token, which is what creates a duplicate account.
func TestDecodeAdminErrorRetryability(t *testing.T) {
	tests := []struct {
		name        string
		status      int
		body        string
		wantRetry   bool
		wantMessage string
	}{
		{
			name:        "in progress is retryable",
			status:      http.StatusConflict,
			body:        `{"error":{"code":"OperationInProgress","message":"in flight"},"requestId":"r"}`,
			wantRetry:   true,
			wantMessage: "OperationInProgress",
		},
		{
			name:        "service unavailable is retryable",
			status:      http.StatusServiceUnavailable,
			body:        `{"error":{"code":"ServiceUnavailable","message":"down"},"requestId":"r"}`,
			wantRetry:   true,
			wantMessage: "ServiceUnavailable",
		},
		{
			name:        "internal error is retryable",
			status:      http.StatusInternalServerError,
			body:        `{"error":{"code":"InternalError","message":"boom"},"requestId":"r"}`,
			wantRetry:   true,
			wantMessage: "InternalError",
		},
		{
			name:        "access denied is not",
			status:      http.StatusForbidden,
			body:        `{"error":{"code":"AccessDenied","message":"no"},"requestId":"r"}`,
			wantMessage: "AccessDenied",
		},
		{
			name:        "idempotent mismatch is not",
			status:      http.StatusBadRequest,
			body:        `{"error":{"code":"IdempotentParameterMismatch","message":"no"},"requestId":"r"}`,
			wantMessage: "IdempotentParameterMismatch",
		},
		{
			name:        "a non-envelope body is reported verbatim",
			status:      http.StatusBadGateway,
			body:        `<html>nginx</html>`,
			wantMessage: "nginx",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := cmd.DecodeAdminError(tc.status, []byte(tc.body))
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.wantMessage)
			assert.Equal(t, tc.wantRetry, cmd.RetryableAdminError(err))
		})
	}
}
