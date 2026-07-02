package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func metadataStub(t *testing.T, enforceV2 bool) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/api/token", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPut {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		_, _ = w.Write([]byte("v2-token"))
	})
	guard := func(w http.ResponseWriter, r *http.Request) bool {
		if enforceV2 && r.Header.Get("X-Aws-Ec2-Metadata-Token") != "v2-token" {
			w.WriteHeader(http.StatusUnauthorized)
			return false
		}
		return true
	}
	mux.HandleFunc("/meta-data/instance-id", func(w http.ResponseWriter, r *http.Request) {
		if guard(w, r) {
			_, _ = w.Write([]byte("i-0abc123"))
		}
	})
	mux.HandleFunc("/meta-data/placement/availability-zone", func(w http.ResponseWriter, r *http.Request) {
		if guard(w, r) {
			_, _ = w.Write([]byte("us-east-1a"))
		}
	})
	mux.HandleFunc("/dynamic/instance-identity/document", func(w http.ResponseWriter, r *http.Request) {
		if guard(w, r) {
			_, _ = w.Write([]byte(`{"accountId":"123456789012","region":"us-east-1"}`))
		}
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func TestFetchInstanceMetadata_OK(t *testing.T) {
	srv := metadataStub(t, true)
	got, err := fetchInstanceMetadata(srv.Client(), srv.URL)
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if got.InstanceID != "i-0abc123" || got.AZ != "us-east-1a" || got.AccountID != "123456789012" {
		t.Errorf("metadata mismatch: %+v", got)
	}
}

func TestFetchInstanceMetadata_TokenRequired(t *testing.T) {
	// Server returns 405 for non-PUT token; a GET-only client would 401 downstream.
	srv := metadataStub(t, true)
	if _, err := fetchInstanceMetadata(srv.Client(), srv.URL); err != nil {
		t.Fatalf("v2 path should succeed: %v", err)
	}
}

func TestFetchInstanceMetadata_BadDocument(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/token", func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte("t")) })
	mux.HandleFunc("/meta-data/instance-id", func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte("i-1")) })
	mux.HandleFunc("/meta-data/placement/availability-zone", func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte("z")) })
	mux.HandleFunc("/dynamic/instance-identity/document", func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte("not json")) })
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	if _, err := fetchInstanceMetadata(srv.Client(), srv.URL); err == nil {
		t.Fatal("expected parse error on bad identity document")
	}
}
