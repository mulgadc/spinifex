package imdscreds

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// imdsStub serves the IMDSv2 token + role + credentials endpoints.
func imdsStub(t *testing.T, creds map[string]string, enforceV2 bool) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/api/token", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPut {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		_, _ = w.Write([]byte("v2-token"))
	})
	mux.HandleFunc("/meta-data/iam/security-credentials/", func(w http.ResponseWriter, r *http.Request) {
		if enforceV2 && r.Header.Get("X-Aws-Ec2-Metadata-Token") != "v2-token" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if r.URL.Path == "/meta-data/iam/security-credentials/" {
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
	exp := time.Now().Add(6 * time.Hour).UTC().Round(time.Second)
	srv := imdsStub(t, map[string]string{
		"Code":            "Success",
		"AccessKeyId":     "AKIATEST",
		"SecretAccessKey": "secrettest",
		"Token":           "sessiontoken",
		"Expiration":      exp.Format(time.RFC3339),
	}, true)

	got, err := Fetch(srv.Client(), srv.URL)
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if got.AccessKeyID != "AKIATEST" || got.SecretAccessKey != "secrettest" || got.SessionToken != "sessiontoken" {
		t.Errorf("creds mismatch: %+v", got)
	}
	if !got.Expiration.Equal(exp) {
		t.Errorf("Expiration = %v, want %v", got.Expiration, exp)
	}
}

func TestFetch_MissingKeys(t *testing.T) {
	srv := imdsStub(t, map[string]string{"Code": "Success"}, false)
	if _, err := Fetch(srv.Client(), srv.URL); err == nil {
		t.Fatal("expected error when access/secret key absent")
	}
}

func TestFetch_BadExpirationIsZero(t *testing.T) {
	srv := imdsStub(t, map[string]string{
		"AccessKeyId":     "AKIA",
		"SecretAccessKey": "secret",
		"Expiration":      "not-a-time",
	}, false)
	got, err := Fetch(srv.Client(), srv.URL)
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if !got.Expiration.IsZero() {
		t.Errorf("Expiration = %v, want zero on parse failure", got.Expiration)
	}
}
