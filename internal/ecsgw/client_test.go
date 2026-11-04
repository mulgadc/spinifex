package ecsgw

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/mulgadc/bluebottle/pkg/sigv4"
	"github.com/mulgadc/spinifex/internal/gwsign"
)

const testSecret = "secret"

// testSigner signs with a fixed temporary credential set (with a session token,
// as IMDS instance-role creds carry).
func testSigner() *gwsign.Signer {
	return gwsign.New(credentials.NewStaticCredentialsProvider("ASIATEST", testSecret, "session-token-xyz"))
}

func newTestClient(t *testing.T, srv *httptest.Server) *Client {
	t.Helper()
	c, err := New(srv.URL, "", testSigner(), "ap-southeast-2", time.Second)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	c.httpClient = srv.Client()
	return c
}

// Call signs for service "ecs", sets the X-Amz-Target action, and returns the
// 2xx body.
func TestClient_CallSignsAndTargets(t *testing.T) {
	var gotTarget, gotAuth, gotBody, gotToken string
	var verifyErr error
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// sigv4.Parse reads and hashes the body itself, then restores it.
		sr, err := sigv4.Parse(r)
		if err == nil {
			_, err = sr.Verify(testSecret, "ap-southeast-2", "ecs")
		}
		verifyErr = err
		gotTarget = r.Header.Get("X-Amz-Target")
		gotAuth = r.Header.Get("Authorization")
		gotToken = r.Header.Get("X-Amz-Security-Token")
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
	if verifyErr != nil {
		t.Fatalf("gateway-side SigV4 verify failed: %v", verifyErr)
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
	if gotToken != "session-token-xyz" {
		t.Errorf("X-Amz-Security-Token = %q, want the IMDS session token", gotToken)
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
	if _, err := New("", "", testSigner(), "r", 0); err == nil {
		t.Error("want error for empty baseURL")
	}
	if _, err := New("https://gw:9999", "", nil, "r", 0); err == nil {
		t.Error("want error for nil signer")
	}
}
