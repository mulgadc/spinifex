package sqsworkergw

import (
	"context"
	"encoding/pem"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

type nopSigner struct{ err error }

func (s nopSigner) Sign(r *http.Request, _, service, region string) error {
	if s.err != nil {
		return s.err
	}
	r.Header.Set("Authorization", "test "+service+"/"+region)
	return nil
}

var fp = strings.Repeat("ab", 32)

func claimBody(inner string) string {
	return `<ClaimReceiptKeyResponse><ClaimReceiptKeyResult>` + inner + `</ClaimReceiptKeyResult></ClaimReceiptKeyResponse>`
}

// server answers with body and status, counting and keeping requests.
func server(t *testing.T, status int, body string) (*Client, *atomic.Int32, *[]string) {
	t.Helper()
	var n atomic.Int32
	var seen []string
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n.Add(1)
		b, _ := io.ReadAll(r.Body)
		seen = append(seen, r.Header.Get("Authorization")+"|"+string(b))
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(srv.Close)
	ca := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw})
	c, err := New(srv.URL, "ap-southeast-2", ca, nopSigner{})
	if err != nil {
		t.Fatal(err)
	}
	return c, &n, &seen
}

func TestNew_RefusesWithoutHTTPSRegionCAOrSigner(t *testing.T) {
	ca := []byte("not a certificate")
	cases := map[string]func() error{
		"http":      func() error { _, err := New("http://10.0.0.1:9999", "ap-southeast-2", ca, nopSigner{}); return err },
		"no host":   func() error { _, err := New("https://", "ap-southeast-2", ca, nopSigner{}); return err },
		"no region": func() error { _, err := New("https://10.0.0.1:9999", "", ca, nopSigner{}); return err },
		"no signer": func() error { _, err := New("https://10.0.0.1:9999", "ap-southeast-2", ca, nil); return err },
		"no CA":     func() error { _, err := New("https://10.0.0.1:9999", "ap-southeast-2", ca, nopSigner{}); return err },
	}
	for name, fn := range cases {
		if fn() == nil {
			t.Errorf("%s: client built", name)
		}
	}
}

func TestClaim_SignsForTheInternalServiceAndRegion(t *testing.T) {
	c, _, seen := server(t, 200, claimBody(`<Pending>true</Pending><HostNodes>1</HostNodes>`))
	got, err := c.ClaimReceiptKey(context.Background())
	if err != nil || !got.Pending || got.Fingerprint != "" || got.HostNodes != 1 {
		t.Fatalf("claim = %+v %v", got, err)
	}
	if req := (*seen)[0]; !strings.HasPrefix(req, "test spinifex-sqs/ap-southeast-2|") || !strings.Contains(req, "Action=ClaimReceiptKey") {
		t.Fatalf("request = %q", req)
	}
}

// Only pending alone, or exactly one valid fingerprint, is a decision.
func TestClaim_RefusesAnythingButOneDecision(t *testing.T) {
	for name, inner := range map[string]string{
		"pending with a key":  `<Pending>true</Pending><Fingerprint>` + fp + `</Fingerprint>`,
		"neither":             `<Pending>false</Pending>`,
		"uppercase key":       `<Pending>false</Pending><Fingerprint>` + strings.ToUpper(fp) + `</Fingerprint>`,
		"short key":           `<Pending>false</Pending><Fingerprint>` + fp[:62] + `</Fingerprint>`,
		"not the claim reply": "",
	} {
		t.Run(name, func(t *testing.T) {
			body := claimBody(inner)
			if inner == "" {
				body = `<Other/>`
			}
			c, _, _ := server(t, 200, body)
			if _, err := c.ClaimReceiptKey(context.Background()); err == nil {
				t.Fatal("accepted a non-decision")
			}
		})
	}
	c, _, _ := server(t, 200, claimBody(`<Pending>false</Pending><Fingerprint>`+fp+`</Fingerprint>`))
	got, err := c.ClaimReceiptKey(context.Background())
	if err != nil || got.Pending || got.Fingerprint != fp {
		t.Fatalf("recorded claim = %+v %v", got, err)
	}
}

func TestRecordReceiptKey_SendsOnlyAFingerprint(t *testing.T) {
	c, n, seen := server(t, 200, `<RecordReceiptKeyResponse/>`)
	const raw = "not-a-fingerprint-and-maybe-key-material"
	if err := c.RecordReceiptKey(context.Background(), raw); err == nil || strings.Contains(err.Error(), raw) {
		t.Fatalf("invalid err = %v", err)
	}
	if n.Load() != 0 {
		t.Fatal("a non-fingerprint was sent")
	}
	if err := c.RecordReceiptKey(context.Background(), fp); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains((*seen)[0], "Fingerprint="+fp) {
		t.Fatalf("request = %q", (*seen)[0])
	}
}

// Refusals carry only the parsed code; ServiceUnavailable is the one
// retryable refusal, and transport failures are retryable too.
func TestErrors_ClassifiedWithoutEchoingTheBody(t *testing.T) {
	cases := []struct {
		status    int
		code      string
		retryable bool
	}{
		{403, "AccessDenied", false},
		{400, "InvalidParameterValue", false},
		{400, "InvalidState", false},
		{403, "SignatureDoesNotMatch", false},
		{503, "ServiceUnavailable", true},
		{500, "InternalFailure", false},
	}
	for _, tc := range cases {
		c, _, _ := server(t, tc.status, `<ErrorResponse><Error><Code>`+tc.code+`</Code><Message>secret-ish detail</Message></Error></ErrorResponse>`)
		_, err := c.ClaimReceiptKey(context.Background())
		api, ok := errors.AsType[*APIError](err)
		if !ok || api.Code != tc.code || api.Retryable() != tc.retryable || api.Pending() != (tc.code == "AccessDenied") {
			t.Fatalf("%s: err = %v", tc.code, err)
		}
		if strings.Contains(err.Error(), "secret-ish") {
			t.Fatal("the error echoes the response body")
		}
	}

	c, _, _ := server(t, 200, "")
	c.baseURL = "https://127.0.0.1:1"
	_, err := c.ClaimReceiptKey(context.Background())
	tr, ok := errors.AsType[*TransientError](err)
	if !ok || !tr.Retryable() {
		t.Fatalf("unreachable err = %v, want TransientError", err)
	}
}

// A redirect would resend a signed request elsewhere; it is not followed.
func TestCall_DoesNotFollowRedirects(t *testing.T) {
	var followed atomic.Bool
	target := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { followed.Store(true) }))
	t.Cleanup(target.Close)
	c, _, _ := server(t, 0, "")
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusTemporaryRedirect)
	}))
	t.Cleanup(srv.Close)
	c.baseURL = srv.URL
	if _, err := c.ClaimReceiptKey(context.Background()); err == nil {
		t.Fatal("a redirect was accepted as a decision")
	}
	if followed.Load() {
		t.Fatal("the signed request followed a redirect")
	}
}
