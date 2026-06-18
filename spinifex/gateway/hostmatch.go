package gateway

import (
	"context"
	"net/http"
	"strings"
)

// hostMatch routes the OCI registry surface (/v2/*) by request Host. The
// registry is addressed at {accountID}.dkr.ecr.{region}.{suffix}; when the Host
// matches, the parsed accountID and region are stashed on the request context
// for the registry handlers and auth bridge. Non-registry hosts pass through
// unchanged — rejection is the auth bridge's responsibility.
func (gw *GatewayConfig) hostMatch(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		accountID, region, ok := parseRegistryHost(r.Host, gw.InternalSuffix)
		if ok {
			ctx := context.WithValue(r.Context(), ctxTargetAccount, accountID)
			ctx = context.WithValue(ctx, ctxTargetRegion, region)
			r = r.WithContext(ctx)
		}
		next.ServeHTTP(w, r)
	})
}

// parseRegistryHost extracts the accountID and region from a registry host of
// the form {accountID}.dkr.ecr.{region}.{suffix}. Any :port is stripped first.
// When suffix is non-empty the host must carry that exact suffix; when suffix
// is empty a permissive parse is used so dev/hosts-file setups still work.
// ok is false for any host that does not match the registry shape.
func parseRegistryHost(host, suffix string) (accountID, region string, ok bool) {
	host = stripPort(host)
	if host == "" {
		return "", "", false
	}

	// minLabels is the label count after any suffix is removed. With a configured
	// suffix the registry host reduces to {accountID}.dkr.ecr.{region} (4 labels);
	// without one the permissive parse needs an extra suffix label (5).
	rest := host
	minLabels := 5
	if suffix != "" {
		trimmed := strings.TrimSuffix(host, "."+suffix)
		if trimmed == host {
			return "", "", false
		}
		rest = trimmed
		minLabels = 4
	}

	// rest must now be {accountID}.dkr.ecr.{region}[.{remainder}].
	labels := strings.Split(rest, ".")
	if len(labels) < minLabels {
		return "", "", false
	}
	if labels[1] != "dkr" || labels[2] != "ecr" {
		return "", "", false
	}
	accountID = labels[0]
	region = labels[3]
	if accountID == "" || region == "" {
		return "", "", false
	}
	return accountID, region, true
}

// stripPort removes a trailing :port from a host, leaving bare IPv6 intact.
func stripPort(host string) string {
	if host == "" {
		return ""
	}
	// Bracketed IPv6 with optional port: [::1]:5000 or [::1].
	if strings.HasPrefix(host, "[") {
		if end := strings.IndexByte(host, ']'); end >= 0 {
			return host[1:end]
		}
		return host
	}
	// Single colon means host:port; multiple colons mean bare IPv6, never a
	// registry host, so leave it untouched.
	if strings.Count(host, ":") == 1 {
		if h, _, found := strings.Cut(host, ":"); found {
			return h
		}
	}
	return host
}
