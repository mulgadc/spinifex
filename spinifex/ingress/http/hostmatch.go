package http

import (
	"context"
	"net"
	"net/http"
	"strings"
)

type targetKey int

const (
	targetAccountKey targetKey = iota
	targetRegionKey
)

// HostMatcher recognises hosts of the form {account}.{labels...}.{region}.{suffix}
// and names no service: the labels come from the composing code.
type HostMatcher struct {
	labels []string
	suffix string
}

// NewHostMatcher returns a matcher for hosts carrying labels between the account and
// the region. With a non-empty suffix the host must end in it; with an empty one any
// trailing domain is accepted, so hosts-file setups still work.
func NewHostMatcher(suffix string, labels ...string) HostMatcher {
	return HostMatcher{labels: append([]string(nil), labels...), suffix: suffix}
}

// Parse extracts the account and region from host, ignoring any :port.
// ok is false for a host that does not match.
func (m HostMatcher) Parse(host string) (account, region string, ok bool) {
	host = stripPort(host)
	if host == "" {
		return "", "", false
	}

	// With a suffix the host reduces to {account}.{labels}.{region}; without one it
	// needs at least one trailing domain label as well.
	rest := host
	minLabels := len(m.labels) + 2
	if m.suffix != "" {
		trimmed := strings.TrimSuffix(host, "."+m.suffix)
		if trimmed == host {
			return "", "", false
		}
		rest = trimmed
	} else {
		minLabels++
	}

	parts := strings.Split(rest, ".")
	if len(parts) < minLabels {
		return "", "", false
	}
	for i, l := range m.labels {
		if parts[1+i] != l {
			return "", "", false
		}
	}
	account = parts[0]
	region = parts[1+len(m.labels)]
	if account == "" || region == "" {
		return "", "", false
	}
	return account, region, true
}

// Middleware stashes the parsed account and region on the request context when the
// Host matches. A non-matching host passes through unchanged; rejecting it is the
// downstream handler's job.
func (m HostMatcher) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if account, region, ok := m.Parse(r.Host); ok {
			r = r.WithContext(WithTarget(r.Context(), account, region))
		}
		next.ServeHTTP(w, r)
	})
}

// WithTarget returns ctx carrying the account and region a host named.
func WithTarget(ctx context.Context, account, region string) context.Context {
	ctx = context.WithValue(ctx, targetAccountKey, account)
	return context.WithValue(ctx, targetRegionKey, region)
}

// TargetAccount is the account the request Host named, empty when it named none.
func TargetAccount(ctx context.Context) string {
	s, _ := ctx.Value(targetAccountKey).(string)
	return s
}

// TargetRegion is the region the request Host named, empty when it named none.
func TargetRegion(ctx context.Context) string {
	s, _ := ctx.Value(targetRegionKey).(string)
	return s
}

// stripPort removes a trailing :port from a host, leaving bare IPv6 intact.
func stripPort(host string) string {
	if h, _, err := net.SplitHostPort(host); err == nil {
		return h
	}
	if h, ok := strings.CutPrefix(host, "["); ok {
		return strings.TrimSuffix(h, "]")
	}
	return host
}
