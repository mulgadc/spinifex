//go:build e2e

package harness

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/mulgadc/spinifex/spinifex/config"

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

// PeerClusterConfig reads and decodes a node's cluster configuration without
// allowing the harness's SPINIFEX_* environment variables to shadow it.
func PeerClusterConfig(t *testing.T, node Node) *config.ClusterConfig {
	t.Helper()
	const path = "/etc/spinifex/spinifex.toml"
	cc, err := loadClusterConfigBytes(PeerFileContents(t, node, path))
	if err != nil {
		t.Fatalf("decode %s on %s: %v", path, node.Name, err)
	}
	return cc
}

// PeerNorthstarDomains returns the public and internal authoritative domains
// mirrored into a peer's local node configuration.
func PeerNorthstarDomains(t *testing.T, node Node) (string, string) {
	t.Helper()
	cc := PeerClusterConfig(t, node)
	local, ok := cc.Nodes[cc.Node]
	if !ok {
		t.Fatalf("cluster config on %s has no local node stanza %q", node.Name, cc.Node)
	}
	base := strings.TrimSpace(local.Northstar.DefaultDomain)
	internal := strings.TrimSpace(local.Northstar.InternalDomain)
	if base == "" || internal == "" {
		t.Fatalf("northstar domains are not fully configured on %s: base=%q internal=%q", node.Name, base, internal)
	}
	return base, internal
}
