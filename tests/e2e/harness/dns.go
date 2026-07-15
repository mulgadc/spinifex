//go:build e2e

package harness

import (
	"path/filepath"

	handlers_dns "github.com/mulgadc/spinifex/spinifex/handlers/dns"
)

// NorthstarBaseDomain returns the cluster's authoritative DNS base domain
// (northstar's default_domain), or "" when DNS registration is not configured.
// It resolves the value the same way the daemon and vpcd do, so tests can
// predict the AWS-shaped names those services serve (ec2-…, …elb…) rather than
// hard-coding a domain the fixture may not use.
//
// Returns "" whenever the config cannot be read: callers must treat that as
// "DNS off" and assert the corresponding fallback branch, which is what the
// services themselves do with an empty base domain.
func NorthstarBaseDomain(env *Env) string {
	if env == nil || env.ConfigDir == "" {
		return ""
	}
	cc, err := loadClusterConfig(filepath.Join(env.ConfigDir, "spinifex.toml"))
	if err != nil {
		return ""
	}
	// The local node's stanza is authoritative; fall back to any node carrying a
	// domain, since the zone is cluster-wide.
	if n, ok := cc.Nodes[cc.Node]; ok {
		if d := handlers_dns.ResolveBaseDomain(&n); d != "" {
			return d
		}
	}
	for _, n := range cc.Nodes {
		if d := handlers_dns.ResolveBaseDomain(&n); d != "" {
			return d
		}
	}
	return ""
}
