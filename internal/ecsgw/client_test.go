package ecsgw

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func newTestClient(t *testing.T, srv *httptest.Server) *Client {
	t.Helper()
	c, err := New(srv.URL, "", "AKIDTEST", "secret", "ap-southeast-2", time.Second)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	c.httpClient = srv.Client()
	return c
}

// Call signs for service "ecs", sets the X-Amz-Target action, and returns the
// 2xx body.
func TestClient_CallSignsAndTargets(t *testing.T) {
	var gotTarget, gotAuth, gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotTarget = r.Header.Get("X-Amz-Target")
		gotAuth = r.Header.Get("Authorization")
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()

	c := newTestClient(t, srv)
	out, err := c.Call("RegisterContainerInstance", []byte(`{"instanceId":"i-1"}`))
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if string(out) != `{"ok":true}` {
		t.Errorf("body = %q", out)
	}
	if gotTarget != targetPrefix+"RegisterContainerInstance" {
		t.Errorf("target = %q", gotTarget)
	}
	if gotBody != `{"instanceId":"i-1"}` {
		t.Errorf("relayed body = %q", gotBody)
	}
	if !strings.Contains(gotAuth, "AWS4-HMAC-SHA256") || !strings.Contains(gotAuth, "/ecs/aws4_request") {
		t.Errorf("auth header not SigV4 for ecs: %q", gotAuth)
	}
}

// A non-2xx gateway response surfaces as an error carrying status + body.
func TestClient_CallErrorStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"__type":"AccessDenied"}`))
	}))
	defer srv.Close()

	c := newTestClient(t, srv)
	if _, err := c.Call("Heartbeat", []byte(`{}`)); err == nil ||
		!strings.Contains(err.Error(), "403") || !strings.Contains(err.Error(), "AccessDenied") {
		t.Fatalf("want 403 AccessDenied error, got %v", err)
	}
}

func TestNew_Validation(t *testing.T) {
	if _, err := New("", "", "a", "b", "r", 0); err == nil {
		t.Error("want error for empty baseURL")
	}
	if _, err := New("https://gw:9999", "", "", "b", "r", 0); err == nil {
		t.Error("want error for empty access key")
	}
}
