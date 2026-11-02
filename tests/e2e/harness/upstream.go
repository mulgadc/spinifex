//go:build e2e

package harness

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"syscall"
	"time"
)

// HTTPStatusError is a host that answered with a non-2xx status. The host is
// up and said no, which is what a retired build or a moved path looks like.
type HTTPStatusError struct {
	Code   int
	Status string
}

func (e *HTTPStatusError) Error() string { return "HTTP " + e.Status }

// HeadContentLength issues a HEAD and returns Content-Length, or an
// *HTTPStatusError for any non-2xx answer.
func HeadContentLength(client *http.Client, rawURL string, timeout time.Duration) (int64, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodHead, rawURL, nil)
	if err != nil {
		return 0, fmt.Errorf("build request: %w", err)
	}
	resp, err := client.Do(req)
	if err != nil {
		return 0, err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return 0, &HTTPStatusError{Code: resp.StatusCode, Status: resp.Status}
	}
	return resp.ContentLength, nil
}

// UpstreamUnavailable reports whether err means a third-party host could not
// be reached or was overloaded (a timeout, a refused or reset connection, a DNS
// failure, a 502/503/504), as opposed to one that answered and said no.
//
// Certificate and TLS failures are never unavailability: a lapsed certificate
// breaks every import as surely as a missing file does.
func UpstreamUnavailable(err error) bool {
	if err == nil {
		return false
	}
	var status *HTTPStatusError
	if errors.As(err, &status) {
		switch status.Code {
		case http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
			return true
		}
		return false
	}
	if isTLSFailure(err) {
		return false
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return true
	}
	if errors.Is(err, syscall.ECONNREFUSED) || errors.Is(err, syscall.ECONNRESET) ||
		errors.Is(err, syscall.EHOSTUNREACH) || errors.Is(err, syscall.ENETUNREACH) {
		return true
	}
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		return true
	}
	var netErr net.Error
	return errors.As(err, &netErr) && netErr.Timeout()
}

func isTLSFailure(err error) bool {
	var (
		verifyErr   *tls.CertificateVerificationError
		alertErr    tls.AlertError
		recordErr   tls.RecordHeaderError
		unknownAuth x509.UnknownAuthorityError
		hostnameErr x509.HostnameError
		invalidCert x509.CertificateInvalidError
	)
	return errors.As(err, &verifyErr) || errors.As(err, &alertErr) || errors.As(err, &recordErr) ||
		errors.As(err, &unknownAuth) || errors.As(err, &hostnameErr) || errors.As(err, &invalidCert)
}
