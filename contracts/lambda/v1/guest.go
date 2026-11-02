// Package lambdav1 holds the Lambda process-contract vocabulary shared by the
// node executor and the guest agent. See README.md for the declarations.
package lambdav1

import (
	"fmt"
	"strings"
)

// ALPNv1 names L-GUEST major version 1; a peer offering any other fails the handshake.
const ALPNv1 = "spinifex-lambda-guest/1"

// FwCfgPrefix prefixes every fw_cfg entry the executor passes the guest.
const FwCfgPrefix = "opt/spinifex/lambda/"

// fw_cfg entry names under FwCfgPrefix.
const (
	EntryGuestKey  = "guest-key"
	EntryGuestCert = "guest-cert"
	EntryHostCert  = "host-cert"
	EntryScope     = "expect"
	EntryHostAddr  = "host-addr"
)

// Roles named as the last URI SAN path segment.
const (
	RoleHost  = "host"
	RoleGuest = "guest"
)

const uriScheme = "spinifex-lambda"

// Scope names one environment generation:
// Region/account/function ID/environment ID/generation.
type Scope string

// ParseScope accepts exactly five non-empty, slash-separated segments.
func ParseScope(s string) (Scope, error) {
	parts := strings.Split(s, "/")
	if len(parts) != 5 {
		return "", fmt.Errorf("lambdav1: scope %q: want 5 segments, got %d", s, len(parts))
	}
	for _, p := range parts {
		if p == "" || strings.ContainsAny(p, " \t\r\n?#%") {
			return "", fmt.Errorf("lambdav1: scope %q: empty or invalid segment", s)
		}
	}
	return Scope(s), nil
}

// URI is the single URI SAN a certificate for role in this scope carries.
func (s Scope) URI(role string) string {
	return uriScheme + "://" + string(s) + "/" + role
}
