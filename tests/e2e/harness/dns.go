//go:build e2e

package harness

import (
	"context"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/mulgadc/spinifex/spinifex/config"

	handlers_dns "github.com/mulgadc/spinifex/spinifex/handlers/dns"
	handlers_imds "github.com/mulgadc/spinifex/spinifex/handlers/imds"
)

const (
	// systemdStubResolver is the loopback address systemd-resolved listens on. It
	// is a forwarder, not a resolver: it appears in /etc/resolv.conf on every
	// systemd-resolved image no matter which nameserver DHCP handed out.
	systemdStubResolver = "127.0.0.53"

	// systemdUplinkResolvConf is the uplink view systemd-resolved maintains
	// alongside the stub, listing the nameservers it actually forwards to.
	systemdUplinkResolvConf = "/run/systemd/resolve/resolv.conf"
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

// NorthstarInternalDomain returns the cluster's authoritative internal DNS
// domain, or "" when DNS registration is not configured.
func NorthstarInternalDomain(env *Env) string {
	if env == nil || env.ConfigDir == "" {
		return ""
	}
	cc, err := loadClusterConfig(filepath.Join(env.ConfigDir, "spinifex.toml"))
	if err != nil {
		return ""
	}
	if n, ok := cc.Nodes[cc.Node]; ok {
		if d := handlers_dns.ResolveInternalDomain(&n); d != "" {
			return d
		}
	}
	for _, n := range cc.Nodes {
		if d := handlers_dns.ResolveInternalDomain(&n); d != "" {
			return d
		}
	}
	return ""
}

// RequireDNSEnabled fails when a fixture expected to provide Northstar DNS has
// no authoritative base domain configured. It returns the configured domain.
func RequireDNSEnabled(t *testing.T, env *Env) string {
	t.Helper()
	domain := NorthstarBaseDomain(env)
	if domain == "" {
		t.Fatalf("fixture requires Northstar DNS, but no authoritative base domain is configured")
	}
	return domain
}

// GuestResolvers returns the nameservers a guest forwards DNS queries to,
// unwrapping the systemd-resolved stub on images that run one.
//
// Where the DHCP-supplied nameserver lands is an image detail. Images whose
// client writes /etc/resolv.conf directly leave it there; systemd-resolved
// images symlink /etc/resolv.conf to a stub advertising 127.0.0.53 and keep the
// leased nameserver as a per-link uplink. Reading /etc/resolv.conf alone
// therefore reports the stub on every such image and reveals nothing about what
// DHCP delivered, so the stub is followed to the uplinks behind it.
func GuestResolvers(ctx context.Context, target SSHTarget) ([]string, error) {
	out, err := RunGuestSSH(ctx, target, "cat /etc/resolv.conf")
	if err != nil {
		return nil, fmt.Errorf("read guest /etc/resolv.conf: %w", err)
	}

	resolvers := resolvConfNameservers(string(out))
	if !slices.Contains(resolvers, systemdStubResolver) {
		return resolvers, nil
	}

	// The stub resolves nothing itself, so drop it in favour of its uplinks.
	resolvers = slices.DeleteFunc(resolvers, func(ip string) bool { return ip == systemdStubResolver })
	uplink, err := RunGuestSSH(ctx, target, "cat "+systemdUplinkResolvConf)
	if err != nil {
		return nil, fmt.Errorf("read guest %s behind the systemd-resolved stub: %w", systemdUplinkResolvConf, err)
	}
	return append(resolvers, resolvConfNameservers(string(uplink))...), nil
}

// resolvConfNameservers returns the addresses on the nameserver lines of
// resolv.conf-formatted text.
func resolvConfNameservers(conf string) []string {
	var nameservers []string
	for _, line := range strings.Split(conf, "\n") {
		if fields := strings.Fields(line); len(fields) >= 2 && fields[0] == "nameserver" {
			nameservers = append(nameservers, fields[1])
		}
	}
	return nameservers
}

// AssertGuestResolver fails unless DHCP pointed the guest at the VPC resolver,
// proving option 6 carried the link-local address the per-tap shim answers on.
// It is retried briefly because a guest reachable over SSH has completed its
// lease, but resolved may not have published the uplink yet.
func AssertGuestResolver(t *testing.T, target SSHTarget) {
	t.Helper()
	EventuallyErr(t, func() error {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		resolvers, err := GuestResolvers(ctx, target)
		if err != nil {
			return err
		}
		if !slices.Contains(resolvers, handlers_imds.VPCDNSServerIP) {
			return fmt.Errorf("guest forwards DNS to %v, want the VPC resolver %s",
				resolvers, handlers_imds.VPCDNSServerIP)
		}
		return nil
	}, 30*time.Second, 3*time.Second)
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
