package main

import (
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// fakeBackend serves both the IMDS role/credentials endpoints and the gateway
// ECR GetAuthorizationToken on one TLS server, routed by request path.
func fakeBackend(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()

	// IMDSv2 token.
	mux.HandleFunc("/latest/api/token", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPut {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		_, _ = w.Write([]byte("imds-v2-token"))
	})
	// IMDS role name.
	mux.HandleFunc("/latest/meta-data/iam/security-credentials/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/latest/meta-data/iam/security-credentials/" {
			if r.Header.Get("X-Aws-Ec2-Metadata-Token") != "imds-v2-token" {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			_, _ = w.Write([]byte("node-role"))
			return
		}
		// Role credentials.
		_ = json.NewEncoder(w).Encode(map[string]string{
			"Code":            "Success",
			"Type":            "AWS-HMAC",
			"AccessKeyId":     "AKIATEST",
			"SecretAccessKey": "secrettest",
			"Token":           "sessiontoken",
			"AccountId":       "123456789012",
		})
	})
	// Gateway ECR GetAuthorizationToken (X-Amz-Target dispatch).
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.Header.Get("X-Amz-Target"), "GetAuthorizationToken") {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		authToken := base64.StdEncoding.EncodeToString([]byte("AWS:testjwt"))
		w.Header().Set("Content-Type", "application/x-amz-json-1.1")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"authorizationData": []map[string]any{{
				"authorizationToken": authToken,
				"proxyEndpoint":      "https://123456789012.dkr.ecr.us-east-1.spinifex.internal",
			}},
		})
	})

	srv := httptest.NewTLSServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func clientTrusting(srv *httptest.Server) *http.Client {
	pool := x509.NewCertPool()
	pool.AddCert(srv.Certificate())
	return &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}}}
}

func TestRun_ReturnsBasicAuth(t *testing.T) {
	srv := fakeBackend(t)
	client := clientTrusting(srv)

	cfg := config{
		GatewayURL: srv.URL,
		Region:     "us-east-1",
		IMDSBase:   srv.URL + "/latest",
	}

	const image = "123456789012.dkr.ecr.us-east-1.spinifex.internal/spinifex-demo:latest"
	reqBody, err := json.Marshal(CredentialProviderRequest{
		APIVersion: credProviderAPIVersion,
		Kind:       "CredentialProviderRequest",
		Image:      image,
	})
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}

	var out bytes.Buffer
	if err := run(bytes.NewReader(reqBody), &out, cfg, client); err != nil {
		t.Fatalf("run: %v", err)
	}

	var resp CredentialProviderResponse
	if err := json.Unmarshal(out.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal response: %v\n%s", err, out.String())
	}

	if resp.Kind != "CredentialProviderResponse" {
		t.Errorf("Kind = %q, want CredentialProviderResponse", resp.Kind)
	}
	if resp.CacheKeyType != "Registry" {
		t.Errorf("CacheKeyType = %q, want Registry", resp.CacheKeyType)
	}

	host := "123456789012.dkr.ecr.us-east-1.spinifex.internal"
	auth, ok := resp.Auth[host]
	if !ok {
		t.Fatalf("auth map missing host %q; got %v", host, resp.Auth)
	}
	if auth.Username != "AWS" {
		t.Errorf("username = %q, want AWS", auth.Username)
	}
	if auth.Password != "testjwt" {
		t.Errorf("password = %q, want testjwt", auth.Password)
	}
}

func TestHostFromImage(t *testing.T) {
	cases := map[string]string{
		"acct.dkr.ecr.us-east-1.spinifex.internal/demo:latest": "acct.dkr.ecr.us-east-1.spinifex.internal",
		"https://acct.dkr.ecr.us-east-1.spinifex.internal":     "acct.dkr.ecr.us-east-1.spinifex.internal",
		"host:9999/repo:tag": "host:9999",
		"plainhost":          "plainhost",
	}
	for in, want := range cases {
		if got := hostFromImage(in); got != want {
			t.Errorf("hostFromImage(%q) = %q, want %q", in, got, want)
		}
	}
}
