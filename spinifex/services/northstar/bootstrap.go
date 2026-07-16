package northstar

import (
	"fmt"
	"log/slog"
	"net"
	"sort"
	"strings"
	"time"

	nsconfig "github.com/mulgadc/northstar/pkg/config"
	"github.com/mulgadc/spinifex/spinifex/config"
)

// baseZoneTXT is the marker TXT record seeded at the apex of the base zone.
const baseZoneTXT = "v=spinifex1"

// Seed retry budget: predastore is ordered first in systemd but may not be
// listening yet (Type=simple). ~60s of retries covers a cold start.
const bootstrapMaxAttempts = 30

// bootstrapRetryDelay is a var so tests can shorten the backoff.
var bootstrapRetryDelay = 2 * time.Second

// BootstrapBaseZone ensures the northstar default_domain zone exists in the S3
// bucket. It is a control-plane action: the seed is written with the system
// predastore credentials (the long-running daemon's own key is read-only), and
// the NS topology is derived from the cluster config. It is a no-op when no
// default_domain or S3 bucket is configured, and never overwrites an existing
// zone.
func BootstrapBaseZone(configPath string, cluster *config.ClusterConfig) error {
	slog.Info("northstar bootstrap: starting base zone check",
		"config_path", configPath, "node", cluster.Node)

	serverCfg, err := nsconfig.LoadServerConfig(configPath)
	if err != nil {
		return fmt.Errorf("load northstar config: %w", err)
	}

	domain := strings.TrimSpace(serverCfg.DefaultDomain)
	slog.Info("northstar bootstrap: loaded northstar config",
		"default_domain", domain,
		"s3_endpoint", serverCfg.S3.Endpoint,
		"s3_bucket", serverCfg.S3.Bucket,
		"s3_region", serverCfg.S3.Region)

	if domain == "" {
		slog.Info("northstar bootstrap: no default_domain set, skipping base zone seed")
		return nil
	}
	if serverCfg.S3.Bucket == "" {
		slog.Info("northstar bootstrap: no s3 bucket configured (filesystem mode), skipping base zone seed")
		return nil
	}

	node, ok := cluster.Nodes[cluster.Node]
	if !ok {
		return fmt.Errorf("node %q not found in cluster config (nodes: %v)", cluster.Node, nodeNames(cluster))
	}
	sysCreds := node.Predastore
	slog.Info("northstar bootstrap: resolved system predastore credentials",
		"node", cluster.Node,
		"access_key_set", sysCreds.AccessKey != "",
		"secret_key_set", sysCreds.SecretKey != "")
	if sysCreds.AccessKey == "" || sysCreds.SecretKey == "" {
		return fmt.Errorf("missing system predastore credentials for base zone seed (node %q)", cluster.Node)
	}

	// Reuse northstar.toml's endpoint/bucket but with the system (read-write)
	// credentials rather than the daemon's read-only key.
	s3cfg := &nsconfig.S3Config{
		Endpoint:  serverCfg.S3.Endpoint,
		Region:    serverCfg.S3.Region,
		Bucket:    serverCfg.S3.Bucket,
		AccessKey: sysCreds.AccessKey,
		SecretKey: sysCreds.SecretKey,
	}

	nameservers := buildNameserverSeeds(cluster)
	slog.Info("northstar bootstrap: ensuring base zone",
		"domain", domain, "nameservers", len(nameservers), "multi_node", len(nameservers) > 1)

	// Seed the public base zone (with the apex TXT marker).
	if err := ensureZone(s3cfg, nsconfig.BaseZoneSeed{
		Domain:      domain,
		Nameservers: nameservers,
		TXT:         []string{baseZoneTXT},
	}); err != nil {
		return err
	}

	// Seed the AWS-parity private zone (compute.internal) so its NS topology is
	// consistent across nodes; the writer would otherwise materialise it lazily
	// on the first instance launch.
	internal := strings.TrimSpace(serverCfg.InternalDomain)
	if internal != "" && internal != domain {
		if err := ensureZone(s3cfg, nsconfig.BaseZoneSeed{
			Domain:      internal,
			Nameservers: nameservers,
		}); err != nil {
			return err
		}
	}
	return nil
}

// ensureZone seeds one zone, retrying while predastore comes up. Predastore is
// ordered before us in systemd, but Type=simple means it may not be accepting
// connections yet, so retry until reachable (or the budget is exhausted) rather
// than leaving the zone unseeded until the next restart.
func ensureZone(s3cfg *nsconfig.S3Config, seed nsconfig.BaseZoneSeed) error {
	var created bool
	var err error
	for attempt := 1; attempt <= bootstrapMaxAttempts; attempt++ {
		created, err = nsconfig.EnsureBaseZone(s3cfg, seed)
		if err == nil {
			break
		}
		slog.Warn("northstar bootstrap: seed attempt failed, retrying",
			"domain", seed.Domain, "attempt", attempt, "max", bootstrapMaxAttempts,
			"endpoint", s3cfg.Endpoint, "error", err)
		time.Sleep(bootstrapRetryDelay)
	}
	if err != nil {
		return fmt.Errorf("seed zone %q after %d attempts: %w", seed.Domain, bootstrapMaxAttempts, err)
	}

	if created {
		slog.Info("northstar bootstrap: zone created", "domain", seed.Domain)
	} else {
		slog.Info("northstar bootstrap: zone already present", "domain", seed.Domain)
	}
	return nil
}

// nodeNames returns the sorted node keys of the cluster, for diagnostics.
func nodeNames(cluster *config.ClusterConfig) []string {
	names := make([]string, 0, len(cluster.Nodes))
	for name := range cluster.Nodes {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// ResolverNameserverIPs returns the WAN IPs of cluster nodes running northstar,
// in the same deterministic order as the seeded nameservers. vpcd's per-tap DNS
// shim uses these as forward targets (northstar's :5300 listener), so internal
// names resolve authoritatively and external names via upstream forwarders.
// Loopback is skipped: a dev/misconfig node with no reachable IP yields an empty
// list, letting the caller fall back to the upstream pool DNS.
func ResolverNameserverIPs(cluster *config.ClusterConfig) []string {
	names := configuredNorthstarNodeNames(cluster)
	ips := make([]string, 0, len(names))
	for _, name := range names {
		ip := nameserverIP(cluster.Nodes[name])
		if ip != "" && ip != "127.0.0.1" {
			ips = append(ips, ip)
		}
	}
	return ips
}

// configuredNorthstarNodeNames returns the deterministically ordered nodes that
// explicitly advertise a northstar configuration.
func configuredNorthstarNodeNames(cluster *config.ClusterConfig) []string {
	var names []string
	for name, node := range cluster.Nodes {
		if node.Northstar.ConfigPath != "" {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	return names
}

// buildNameserverSeeds derives one nameserver (nsN → node IP) per cluster node
// that runs northstar, ordered deterministically. Falls back to the local node
// when no node advertises a northstar config (single-node / dev).
func buildNameserverSeeds(cluster *config.ClusterConfig) []nsconfig.NameserverSeed {
	names := configuredNorthstarNodeNames(cluster)
	if len(names) == 0 {
		names = []string{cluster.Node}
	}

	seeds := make([]nsconfig.NameserverSeed, 0, len(names))
	for i, name := range names {
		seeds = append(seeds, nsconfig.NameserverSeed{
			Host: fmt.Sprintf("ns%d", i+1),
			IP:   nameserverIP(cluster.Nodes[name]),
		})
	}
	return seeds
}

// nameserverIP returns a reachable DNS address for a node: its advertised IP,
// else its host, with any port stripped. A missing or wildcard address falls
// back to loopback for single-node/dev.
func nameserverIP(node config.Config) string {
	ip := strings.TrimSpace(node.AdvertiseIP)
	if ip == "" {
		ip = strings.TrimSpace(node.Host)
	}
	if host, _, err := net.SplitHostPort(ip); err == nil {
		ip = host
	}
	if ip == "" || ip == "0.0.0.0" {
		ip = "127.0.0.1"
	}
	return ip
}
