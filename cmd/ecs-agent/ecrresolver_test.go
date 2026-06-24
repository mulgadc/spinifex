package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mulgadc/spinifex/cmd/ecs-agent/credentials"
)

// stubCreds is a credentials.CredentialsProvider returning fixed creds or error.
type stubCreds struct {
	c   credentials.Credentials
	err error
}

func (s stubCreds) Retrieve(context.Context) (credentials.Credentials, error) {
	return s.c, s.err
}

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

func trusting(srv *httptest.Server) *http.Client {
	pool := x509.NewCertPool()
	pool.AddCert(srv.Certificate())
	return &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}}}
}

func TestECRResolver_Authorize(t *testing.T) {
	srv := gatewayStub(t, "AWS:jwt", "https://123456789012.dkr.ecr.us-east-1.spinifex.internal", http.StatusOK)
	creds := stubCreds{c: credentials.Credentials{
		AccessKeyID: "AKIA", SecretAccessKey: "secret", SessionToken: "sess",
		Expiration: time.Now().Add(time.Hour),
	}}
	r := newECRResolver(creds, "us-east-1", srv.URL, trusting(srv))

	user, pass, endpoint, err := r.Authorize(context.Background(), "123456789012.dkr.ecr.us-east-1.spinifex.internal/app:1")
	if err != nil {
		t.Fatalf("Authorize: %v", err)
	}
	if user != "AWS" || pass != "jwt" {
		t.Errorf("got %q:%q, want AWS:jwt", user, pass)
	}
	if endpoint != "123456789012.dkr.ecr.us-east-1.spinifex.internal" {
		t.Errorf("endpoint = %q", endpoint)
	}
}

func TestECRResolver_CredsError(t *testing.T) {
	r := newECRResolver(stubCreds{err: errors.New("no imds")}, "us-east-1", "https://x", http.DefaultClient)
	if _, _, _, err := r.Authorize(context.Background(), "ref"); err == nil {
		t.Fatal("expected creds error to propagate")
	}
}

func TestECRResolver_GatewayError(t *testing.T) {
	srv := gatewayStub(t, "", "", http.StatusInternalServerError)
	creds := stubCreds{c: credentials.Credentials{AccessKeyID: "A", SecretAccessKey: "B"}}
	r := newECRResolver(creds, "us-east-1", srv.URL, trusting(srv))
	if _, _, _, err := r.Authorize(context.Background(), "ref"); err == nil {
		t.Fatal("expected gateway error")
	}
}

func TestECRResolver_LazyClient_BadCADeferredToAuthorize(t *testing.T) {
	creds := stubCreds{c: credentials.Credentials{AccessKeyID: "A", SecretAccessKey: "B"}}
	// Construction with a bogus CA path must not fail — only Authorize does.
	r := newLazyECRResolver(creds, "us-east-1", "https://gw", filepath.Join(t.TempDir(), "absent.pem"))
	if _, _, _, err := r.Authorize(context.Background(), "ref"); err == nil {
		t.Fatal("expected lazy gateway-client build to fail on missing CA")
	}
}
