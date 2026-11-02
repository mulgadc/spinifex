//go:build e2e

package harness

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"syscall"
	"testing"
	"time"
)

func TestUpstreamUnavailableSeparatesOutagesFromBreakage(t *testing.T) {
	head := func(err error) error { return &url.Error{Op: "Head", URL: "https://mirror.example/x", Err: err} }
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"context deadline", head(context.DeadlineExceeded), true},
		{"connection refused", head(&net.OpError{Op: "dial", Net: "tcp", Err: syscall.ECONNREFUSED}), true},
		{"connection reset", head(&net.OpError{Op: "read", Net: "tcp", Err: syscall.ECONNRESET}), true},
		{"dns failure", head(&net.DNSError{Err: "no such host", Name: "mirror.example", IsNotFound: true}), true},
		{"503", &HTTPStatusError{Code: 503, Status: "503 Service Unavailable"}, true},
		{"504", &HTTPStatusError{Code: 504, Status: "504 Gateway Timeout"}, true},
		{"404", &HTTPStatusError{Code: 404, Status: "404 Not Found"}, false},
		{"403", &HTTPStatusError{Code: 403, Status: "403 Forbidden"}, false},
		{"500", &HTTPStatusError{Code: 500, Status: "500 Internal Server Error"}, false},
		{"unknown authority", head(x509.UnknownAuthorityError{}), false},
		{"tls alert over a net error", head(&net.OpError{Op: "remote error", Err: tls.AlertError(42)}), false},
		{"refused redirect", head(errors.New("refusing non-https redirect to http://mirror.example/x")), false},
		{"wrapped status", fmt.Errorf("checksum: %w", &HTTPStatusError{Code: 404, Status: "404 Not Found"}), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := UpstreamUnavailable(tc.err); got != tc.want {
				t.Fatalf("UpstreamUnavailable(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

// The two outcomes the catalog test has to tell apart, from a real round trip:
// a host too slow to answer is an outage, a host that answers 404 is breakage.
func TestHeadContentLengthClassifiesRealResponses(t *testing.T) {
	release := make(chan struct{})
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	defer slow.Close()
	defer close(release)

	missing := httptest.NewServer(http.NotFoundHandler())
	defer missing.Close()

	ok := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Length", "1234")
	}))
	defer ok.Close()

	_, err := HeadContentLength(http.DefaultClient, slow.URL, 50*time.Millisecond)
	if !UpstreamUnavailable(err) {
		t.Errorf("a HEAD that timed out was not classed as an outage: %v", err)
	}

	_, err = HeadContentLength(http.DefaultClient, missing.URL, time.Second)
	var status *HTTPStatusError
	if !errors.As(err, &status) || status.Code != http.StatusNotFound {
		t.Fatalf("a 404 did not come back as an HTTPStatusError: %v", err)
	}
	if UpstreamUnavailable(err) {
		t.Errorf("a 404 was classed as an outage, so a retired catalog entry would be skipped: %v", err)
	}

	size, err := HeadContentLength(http.DefaultClient, ok.URL, time.Second)
	if err != nil || size != 1234 {
		t.Errorf("HeadContentLength = %d, %v; want 1234, nil", size, err)
	}
}
