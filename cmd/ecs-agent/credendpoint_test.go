package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mulgadc/spinifex/internal/stsauth"
)

// newTestCredEndpoint builds an endpoint whose assume seam returns canned creds,
// counting calls so cache behaviour is observable. run is nil (no iface plumbing).
func newTestCredEndpoint(creds stsauth.Credentials) (*credEndpoint, *int32) {
	c := newCredEndpoint(nil, "us-east-1", "https://gw", "", "127.0.0.1", 0, nil)
	var calls int32
	c.assume = func(ctx context.Context, roleARN, sessionName string) (stsauth.Credentials, error) {
		atomic.AddInt32(&calls, 1)
		return creds, nil
	}
	return c, &calls
}

func TestCredEndpoint_ServesRegisteredCredID(t *testing.T) {
	exp := time.Now().Add(time.Hour).UTC().Truncate(time.Second)
	c, _ := newTestCredEndpoint(stsauth.Credentials{
		AccessKeyID: "AKIA", SecretAccessKey: "secret", SessionToken: "tok", Expiration: exp,
	})
	c.Register("cred-1", "arn:aws:iam::111122223333:role/task")

	rr := httptest.NewRecorder()
	c.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/v2/credentials/cred-1", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	var got credResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.AccessKeyID != "AKIA" || got.SecretAccessKey != "secret" || got.Token != "tok" {
		t.Errorf("bad creds in response: %+v", got)
	}
	if got.RoleArn != "arn:aws:iam::111122223333:role/task" {
		t.Errorf("RoleArn = %q", got.RoleArn)
	}
	if got.Expiration != exp.Format(time.RFC3339) {
		t.Errorf("Expiration = %q, want %q", got.Expiration, exp.Format(time.RFC3339))
	}
}

func TestCredEndpoint_UnknownCredIDIs404(t *testing.T) {
	c, calls := newTestCredEndpoint(stsauth.Credentials{AccessKeyID: "x", SecretAccessKey: "y"})
	rr := httptest.NewRecorder()
	c.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/v2/credentials/nope", nil))
	if rr.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rr.Code)
	}
	if *calls != 0 {
		t.Errorf("assume called %d times for unknown credID, want 0", *calls)
	}
}

func TestCredEndpoint_WrongMethodIs405(t *testing.T) {
	c, _ := newTestCredEndpoint(stsauth.Credentials{AccessKeyID: "x", SecretAccessKey: "y"})
	c.Register("cred-1", "arn:role")
	rr := httptest.NewRecorder()
	c.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/v2/credentials/cred-1", nil))
	if rr.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", rr.Code)
	}
}

func TestCredEndpoint_CachesUntilExpiry(t *testing.T) {
	c, calls := newTestCredEndpoint(stsauth.Credentials{
		AccessKeyID: "AKIA", SecretAccessKey: "secret", Expiration: time.Now().Add(time.Hour),
	})
	c.Register("cred-1", "arn:role")
	for hit := range 3 {
		rr := httptest.NewRecorder()
		c.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/v2/credentials/cred-1", nil))
		if rr.Code != http.StatusOK {
			t.Fatalf("hit %d: status %d", hit, rr.Code)
		}
	}
	if *calls != 1 {
		t.Errorf("assume called %d times, want 1 (cached)", *calls)
	}
}

func TestCredEndpoint_RefreshesNearExpiry(t *testing.T) {
	// Expiry inside the refresh margin -> every hit re-assumes.
	c, calls := newTestCredEndpoint(stsauth.Credentials{
		AccessKeyID: "AKIA", SecretAccessKey: "secret", Expiration: time.Now().Add(time.Minute),
	})
	c.Register("cred-1", "arn:role")
	for range 2 {
		rr := httptest.NewRecorder()
		c.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/v2/credentials/cred-1", nil))
	}
	if *calls != 2 {
		t.Errorf("assume called %d times, want 2 (expiry within margin)", *calls)
	}
}

func TestCredEndpoint_DeregisterDropsCredID(t *testing.T) {
	c, _ := newTestCredEndpoint(stsauth.Credentials{AccessKeyID: "x", SecretAccessKey: "y"})
	c.Register("cred-1", "arn:role")
	c.Deregister("cred-1")
	rr := httptest.NewRecorder()
	c.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/v2/credentials/cred-1", nil))
	if rr.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 after deregister", rr.Code)
	}
}

func TestCredEndpoint_AssumeFailureIs502(t *testing.T) {
	c, _ := newTestCredEndpoint(stsauth.Credentials{})
	c.assume = func(ctx context.Context, roleARN, sessionName string) (stsauth.Credentials, error) {
		return stsauth.Credentials{}, context.DeadlineExceeded
	}
	c.Register("cred-1", "arn:role")
	rr := httptest.NewRecorder()
	c.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/v2/credentials/cred-1", nil))
	if rr.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", rr.Code)
	}
}

func TestCredEndpoint_StartServesOverLoopback(t *testing.T) {
	c, _ := newTestCredEndpoint(stsauth.Credentials{
		AccessKeyID: "AKIA", SecretAccessKey: "secret", Expiration: time.Now().Add(time.Hour),
	})
	c.Register("cred-1", "arn:role")
	c.port = 0 // bind an ephemeral port instead of privileged 80
	if err := c.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	defer c.Stop()

	url := "http://" + c.ln.Addr().String() + credRelativeURI("cred-1")
	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
}

func TestCredRelativeURI(t *testing.T) {
	if got := credRelativeURI("abc"); got != "/v2/credentials/abc" {
		t.Errorf("credRelativeURI = %q", got)
	}
}
