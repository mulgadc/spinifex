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
