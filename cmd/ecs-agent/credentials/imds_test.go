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

func imdsStub(t *testing.T, hits *int32, creds map[string]string) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/api/token", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("v2-token"))
	})
	mux.HandleFunc("/meta-data/iam/security-credentials/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/meta-data/iam/security-credentials/" {
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
	exp := time.Now().Add(6 * time.Hour).UTC().Round(time.Second)
	srv := imdsStub(t, &hits, map[string]string{
		"AccessKeyId":     "AKIA",
		"SecretAccessKey": "secret",
		"Token":           "session",
		"Expiration":      exp.Format(time.RFC3339),
	})

	p := NewIMDSProvider(srv.Client(), srv.URL)
	got, err := p.Retrieve(context.Background())
	if err != nil {
		t.Fatalf("Retrieve: %v", err)
	}
	if got.AccessKeyID != "AKIA" || !got.Expiration.Equal(exp) {
		t.Errorf("creds mismatch: %+v", got)
	}

	// Second call within validity window must not re-hit IMDS.
	if _, err := p.Retrieve(context.Background()); err != nil {
		t.Fatalf("Retrieve 2: %v", err)
	}
	if hits != 1 {
		t.Errorf("IMDS credential hits = %d, want 1 (cached)", hits)
	}
}

func TestRetrieve_RefetchesWhenStale(t *testing.T) {
	var hits int32
	// Expiry inside the refresh margin → never cached, always refetched.
	srv := imdsStub(t, &hits, map[string]string{
		"AccessKeyId":     "AKIA",
		"SecretAccessKey": "secret",
		"Expiration":      time.Now().Add(time.Minute).UTC().Format(time.RFC3339),
	})
	p := NewIMDSProvider(srv.Client(), srv.URL)
	if _, err := p.Retrieve(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Retrieve(context.Background()); err != nil {
		t.Fatal(err)
	}
	if hits != 2 {
		t.Errorf("hits = %d, want 2 (stale refetch)", hits)
	}
}

func TestRetrieve_CancelledContext(t *testing.T) {
	srv := imdsStub(t, nil, map[string]string{"AccessKeyId": "A", "SecretAccessKey": "B"})
	p := NewIMDSProvider(srv.Client(), srv.URL)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := p.Retrieve(ctx); err == nil {
		t.Fatal("expected error from cancelled context")
	}
}

func TestCredentials_Valid(t *testing.T) {
	if (Credentials{}).Valid(0) {
		t.Error("empty creds should be invalid")
	}
	noExp := Credentials{AccessKeyID: "a", SecretAccessKey: "b"}
	if !noExp.Valid(time.Hour) {
		t.Error("zero expiry should be treated valid")
	}
	soon := Credentials{AccessKeyID: "a", SecretAccessKey: "b", Expiration: time.Now().Add(time.Minute)}
	if soon.Valid(5 * time.Minute) {
		t.Error("expiry inside margin should be invalid")
	}
}
