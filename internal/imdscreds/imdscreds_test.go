package imdscreds

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
)

// imdsStub serves the IMDSv2 token + role + credentials endpoints under the
// real /latest prefix the SDK IMDS client always requests.
func imdsStub(t *testing.T, creds map[string]string, enforceV2 bool) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/latest/api/token", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPut {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		// Real IMDS echoes the requested TTL back as a response header; the SDK
		// client parses it from there, not the body.
		w.Header().Set("X-Aws-Ec2-Metadata-Token-Ttl-Seconds", "21600")
		_, _ = w.Write([]byte("v2-token"))
	})
	mux.HandleFunc("/latest/meta-data/iam/security-credentials/", func(w http.ResponseWriter, r *http.Request) {
		if enforceV2 && r.Header.Get("X-Aws-Ec2-Metadata-Token") != "v2-token" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if r.URL.Path == "/latest/meta-data/iam/security-credentials/" {
			_, _ = w.Write([]byte("node-role"))
			return
		}
		_ = json.NewEncoder(w).Encode(creds)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func TestFetch_OK(t *testing.T) {
	// Kept under an hour: ec2rolecreds caps Expires to now+1h, so anything
	// further out would not round-trip exactly.
	exp := time.Now().Add(30 * time.Minute).UTC().Round(time.Second)
	srv := imdsStub(t, map[string]string{
		"Code":            "Success",
		"AccessKeyId":     "AKIATEST",
		"SecretAccessKey": "secrettest",
		"Token":           "sessiontoken",
		"Expiration":      exp.Format(time.RFC3339),
	}, true)

	got, err := Fetch(srv.Client(), srv.URL+"/latest")
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if got.AccessKeyID != "AKIATEST" || got.SecretAccessKey != "secrettest" || got.SessionToken != "sessiontoken" {
		t.Errorf("creds mismatch: %+v", got)
	}
	if !got.Expires.Equal(exp) {
		t.Errorf("Expires = %v, want %v", got.Expires, exp)
	}
}

func TestFetch_BaseWithoutLatestSuffix(t *testing.T) {
	// base URLs without a /latest suffix must keep working too — the SDK
	// endpoint is always the scheme+host root regardless.
	exp := time.Now().Add(30 * time.Minute).UTC().Round(time.Second)
	srv := imdsStub(t, map[string]string{
		"Code":            "Success",
		"AccessKeyId":     "AKIATEST",
		"SecretAccessKey": "secrettest",
		"Expiration":      exp.Format(time.RFC3339),
	}, true)

	got, err := Fetch(srv.Client(), srv.URL)
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if got.AccessKeyID != "AKIATEST" {
		t.Errorf("AccessKeyID = %q, want AKIATEST", got.AccessKeyID)
	}
}

func TestFetch_MissingKeys(t *testing.T) {
	srv := imdsStub(t, map[string]string{"Code": "Success"}, false)
	if _, err := Fetch(srv.Client(), srv.URL+"/latest"); err == nil {
		t.Fatal("expected error when access/secret key absent")
	}
}

func TestFetch_CodeNotSuccessErrors(t *testing.T) {
	// A non-Success Code (e.g. the role is mid-rotation) must fail the fetch
	// rather than being silently ignored.
	srv := imdsStub(t, map[string]string{
		"Code":            "Failure",
		"AccessKeyId":     "AKIA",
		"SecretAccessKey": "secret",
	}, false)
	if _, err := Fetch(srv.Client(), srv.URL+"/latest"); err == nil {
		t.Fatal("expected error when Code != Success")
	}
}

func TestFetch_BadExpirationErrors(t *testing.T) {
	// An unparseable Expiration must be a hard error, not silently treated as
	// "never expires" — that was the defect in the hand-rolled implementation.
	srv := imdsStub(t, map[string]string{
		"Code":            "Success",
		"AccessKeyId":     "AKIA",
		"SecretAccessKey": "secret",
		"Expiration":      "not-a-time",
	}, false)
	if _, err := Fetch(srv.Client(), srv.URL+"/latest"); err == nil {
		t.Fatal("expected error on unparseable Expiration")
	}
}

// The provider must not carry ec2rolecreds' extend-on-failure strategy.
// aws.CredentialsCache selects it by asserting on the dynamic type, so this
// assertion is the property that keeps a failed refresh an error.
func TestNewProvider_WithholdsExtendOnFailure(t *testing.T) {
	p := NewProvider(nil, "http://169.254.169.254/latest")

	if _, ok := p.(aws.HandleFailRefreshCredentialsCacheStrategy); ok {
		t.Error("provider implements HandleFailRefreshCredentialsCacheStrategy, so a failed refresh will silently extend a dead credential's expiry")
	}
	if _, ok := p.(aws.AdjustExpiresByCredentialsCacheStrategy); !ok {
		t.Error("provider dropped AdjustExpiresBy, which a caller using an ExpiryWindow relies on")
	}
}

// Behind aws.CredentialsCache, as the ecs-agent uses it, a refresh that fails
// against an expired credential must surface as an error rather than hand back
// the dead key with a pushed-out expiry.
func TestNewProvider_CachedFailedRefreshErrors(t *testing.T) {
	var hits atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("/latest/api/token", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Aws-Ec2-Metadata-Token-Ttl-Seconds", "21600")
		_, _ = w.Write([]byte("v2-token"))
	})
	mux.HandleFunc("/latest/meta-data/iam/security-credentials/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/latest/meta-data/iam/security-credentials/" {
			_, _ = w.Write([]byte("node-role"))
			return
		}
		if hits.Add(1) > 1 {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]string{
			"Code":            "Success",
			"AccessKeyId":     "AKIA",
			"SecretAccessKey": "secret",
			"Expiration":      time.Now().Add(-time.Minute).UTC().Format(time.RFC3339),
		})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	cache := aws.NewCredentialsCache(NewProvider(srv.Client(), srv.URL+"/latest"))
	first, err := cache.Retrieve(context.Background())
	if err != nil {
		t.Fatalf("first Retrieve: %v", err)
	}
	got, err := cache.Retrieve(context.Background())
	if err == nil {
		t.Fatalf("refresh failure was reported as success: %+v", got)
	}
	if got.Expires.After(first.Expires) {
		t.Errorf("expiry extended on refresh failure: %v -> %v", first.Expires, got.Expires)
	}
}
