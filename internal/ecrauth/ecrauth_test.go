package ecrauth

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestHostFromImage(t *testing.T) {
	cases := map[string]string{
		"acct.dkr.ecr.us-east-1.spinifex.internal/demo:latest": "acct.dkr.ecr.us-east-1.spinifex.internal",
		"https://acct.dkr.ecr.us-east-1.spinifex.internal":     "acct.dkr.ecr.us-east-1.spinifex.internal",
		"host:9999/repo:tag": "host:9999",
		"plainhost":          "plainhost",
	}
	for in, want := range cases {
		if got := HostFromImage(in); got != want {
			t.Errorf("HostFromImage(%q) = %q, want %q", in, got, want)
		}
	}
}

// gatewayStub serves the ECR GetAuthorizationToken X-Amz-Target dispatch,
// returning a base64 AWS:<jwt> token like the real gateway.
func gatewayStub(t *testing.T, token, proxy string, status int) *httptest.Server {
	t.Helper()
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.Header.Get("X-Amz-Target"), "GetAuthorizationToken") {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if status != http.StatusOK {
			w.WriteHeader(status)
			return
		}
		w.Header().Set("Content-Type", "application/x-amz-json-1.1")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"authorizationData": []map[string]any{{
				"authorizationToken": base64.StdEncoding.EncodeToString([]byte(token)),
				"proxyEndpoint":      proxy,
			}},
		})
	}))
	t.Cleanup(srv.Close)
	return srv
}

func clientTrusting(srv *httptest.Server) *http.Client {
	pool := x509.NewCertPool()
	pool.AddCert(srv.Certificate())
	return &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}}}
}

func TestGetAuthorizationToken_OK(t *testing.T) {
	srv := gatewayStub(t, "AWS:testjwt", "https://123456789012.dkr.ecr.us-east-1.spinifex.internal", http.StatusOK)

	tok, err := GetAuthorizationToken("us-east-1", srv.URL, clientTrusting(srv), "AKIA", "secret", "session")
	if err != nil {
		t.Fatalf("GetAuthorizationToken: %v", err)
	}
	if tok.Username != "AWS" || tok.Password != "testjwt" {
		t.Errorf("got %q:%q, want AWS:testjwt", tok.Username, tok.Password)
	}
	if tok.ProxyHost != "123456789012.dkr.ecr.us-east-1.spinifex.internal" {
		t.Errorf("ProxyHost = %q", tok.ProxyHost)
	}
}

func TestGetAuthorizationToken_BadTokenForm(t *testing.T) {
	srv := gatewayStub(t, "no-colon-token", "https://x", http.StatusOK)
	if _, err := GetAuthorizationToken("us-east-1", srv.URL, clientTrusting(srv), "AKIA", "secret", ""); err == nil {
		t.Fatal("expected error for token not in user:password form")
	}
}

func TestGetAuthorizationToken_ServerError(t *testing.T) {
	srv := gatewayStub(t, "", "", http.StatusInternalServerError)
	if _, err := GetAuthorizationToken("us-east-1", srv.URL, clientTrusting(srv), "AKIA", "secret", ""); err == nil {
		t.Fatal("expected error on gateway 500")
	}
}

func TestGatewayHTTPClient(t *testing.T) {
	// Empty CA path: system trust store, no error.
	if _, err := GatewayHTTPClient(""); err != nil {
		t.Fatalf("empty CA path: %v", err)
	}
	// Missing file: error.
	if _, err := GatewayHTTPClient(filepath.Join(t.TempDir(), "nope.pem")); err == nil {
		t.Fatal("expected error for missing CA file")
	}
	// Garbage PEM: error.
	bad := filepath.Join(t.TempDir(), "bad.pem")
	if err := os.WriteFile(bad, []byte("not a cert"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := GatewayHTTPClient(bad); err == nil {
		t.Fatal("expected error for CA file with no certificates")
	}
}
