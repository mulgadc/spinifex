package ochre

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/url"
)

// newNoRedirectHTTPClient returns a provider client that hands any redirect
// back as the response, since following one would resend tenant input to
// another host.
func newNoRedirectHTTPClient() *http.Client {
	return &http.Client{
		Timeout:       providerHTTPTimeout,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

// endpointLogID names baseURL by scheme and host only, dropping any userinfo,
// path or query.
func endpointLogID(baseURL string) string {
	u, err := url.Parse(baseURL)
	if err != nil || u.Host == "" {
		return "unparseable"
	}
	return u.Scheme + "://" + u.Host
}

// transportFailureClass reduces a transport error to a fixed label, since
// its text can quote the request URL.
func transportFailureClass(err error) string {
	var netErr net.Error
	switch {
	case errors.Is(err, context.Canceled):
		return "canceled"
	case errors.Is(err, context.DeadlineExceeded), errors.As(err, &netErr) && netErr.Timeout():
		return "timeout"
	default:
		return "unreachable"
	}
}
