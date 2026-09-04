package credentials

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// imdsStub serves the IMDSv2 token + role + credentials endpoints under the
// real /latest prefix the SDK IMDS client always requests.
func imdsStub(t *testing.T, hits *int32, creds map[string]string) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/latest/api/token", func(w http.ResponseWriter, r *http.Request) {
		// Real IMDS echoes the requested TTL back as a response header; the SDK
		// client parses it from there, not the body.
		w.Header().Set("X-Aws-Ec2-Metadata-Token-Ttl-Seconds", "21600")
		_, _ = w.Write([]byte("v2-token"))
	})
	mux.HandleFunc("/latest/meta-data/iam/security-credentials/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/latest/meta-data/iam/security-credentials/" {
			_, _ = w.Write([]byte("node-role"))
			return
		}
		if hits != nil {
			atomic.AddInt32(hits, 1)
		}
		_ = json.NewEncoder(w).Encode(creds)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func TestRetrieve_FetchesAndCaches(t *testing.T) {
	var hits int32
	// Kept under an hour: ec2rolecreds caps Expires to now+1h.
	exp := time.Now().Add(30 * time.Minute).UTC().Round(time.Second)
	srv := imdsStub(t, &hits, map[string]string{
		"Code":            "Success",
		"AccessKeyId":     "AKIA",
		"SecretAccessKey": "secret",
		"Token":           "session",
		"Expiration":      exp.Format(time.RFC3339),
	})

	p := NewIMDSProvider(srv.Client(), srv.URL+"/latest")
	got, err := p.Retrieve(context.Background())
	if err != nil {
		t.Fatalf("Retrieve: %v", err)
	}
	if got.AccessKeyID != "AKIA" || !got.Expiration.Equal(exp) {
		t.Errorf("creds mismatch: %+v", got)
	}

	// Second call well within validity must not re-hit IMDS.
	if _, err := p.Retrieve(context.Background()); err != nil {
		t.Fatalf("Retrieve 2: %v", err)
	}
	if hits != 1 {
		t.Errorf("IMDS credential hits = %d, want 1 (cached)", hits)
	}
}

func TestRetrieve_RefetchesWhenExpired(t *testing.T) {
	var hits int32
	// Already expired: aws.CredentialsCache refetches once the stored
	// credentials' real expiry has passed, regardless of ExpiryWindow.
	srv := imdsStub(t, &hits, map[string]string{
		"Code":            "Success",
		"AccessKeyId":     "AKIA",
		"SecretAccessKey": "secret",
		"Expiration":      time.Now().Add(-time.Minute).UTC().Format(time.RFC3339),
	})
	p := NewIMDSProvider(srv.Client(), srv.URL+"/latest")
	if _, err := p.Retrieve(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Retrieve(context.Background()); err != nil {
		t.Fatal(err)
	}
	if hits != 2 {
		t.Errorf("hits = %d, want 2 (expired refetch)", hits)
	}
}

// imdsStubFailAfterFirst serves creds on the first credential request and 500s
// on every one after it, so a test can drive a refresh that fails against a
// credential the cache has already stored.
func imdsStubFailAfterFirst(t *testing.T, creds map[string]string) *httptest.Server {
	t.Helper()
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
		_ = json.NewEncoder(w).Encode(creds)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// A refresh that fails must surface as an error. Left to itself the SDK's
// ec2rolecreds implements HandleFailToRefresh, which hands back the same dead
// key with Expires pushed 5-15 minutes out, so the agent would sign requests
// with a credential the control plane has already rejected — indefinitely,
// since every subsequent refresh fails the same way.
func TestRetrieve_FailedRefreshDoesNotExtendExpiry(t *testing.T) {
	srv := imdsStubFailAfterFirst(t, map[string]string{
		"Code":            "Success",
		"AccessKeyId":     "AKIA",
		"SecretAccessKey": "secret",
		"Token":           "session",
		// Already expired, so the next Retrieve must go back to IMDS.
		"Expiration": time.Now().Add(-time.Minute).UTC().Format(time.RFC3339),
	})

	p := NewIMDSProvider(srv.Client(), srv.URL+"/latest")
	first, err := p.Retrieve(context.Background())
	if err != nil {
		t.Fatalf("first Retrieve: %v", err)
	}

	got, err := p.Retrieve(context.Background())
	if err == nil {
		t.Fatalf("refresh failure was reported as success: %+v", got)
	}
	if got.Expiration.After(first.Expiration) {
		t.Errorf("expiry extended on refresh failure: %v -> %v", first.Expiration, got.Expiration)
	}
}

func TestRetrieve_CancelledContext(t *testing.T) {
	srv := imdsStub(t, nil, map[string]string{"Code": "Success", "AccessKeyId": "A", "SecretAccessKey": "B"})
	p := NewIMDSProvider(srv.Client(), srv.URL+"/latest")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := p.Retrieve(ctx); err == nil {
		t.Fatal("expected error from cancelled context")
	}
}

func TestRetrieve_CodeNotSuccessErrors(t *testing.T) {
	srv := imdsStub(t, nil, map[string]string{"Code": "Failure", "AccessKeyId": "A", "SecretAccessKey": "B"})
	p := NewIMDSProvider(srv.Client(), srv.URL+"/latest")
	if _, err := p.Retrieve(context.Background()); err == nil {
		t.Fatal("expected error when Code != Success")
	}
}
