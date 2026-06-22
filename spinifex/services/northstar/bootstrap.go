package northstar

import (
	"fmt"
	"log/slog"
	"net"
	"sort"
	"strings"

	nsconfig "github.com/mulgadc/northstar/pkg/config"
	"github.com/mulgadc/spinifex/spinifex/config"
)

// baseZoneTXT is the marker TXT record seeded at the apex of the base zone.
const baseZoneTXT = "v=spinifex1"

// BootstrapBaseZone ensures the northstar default_domain zone exists in the S3
// bucket. It is a control-plane action: the seed is written with the system
// predastore credentials (the long-running daemon's own key is read-only), and
// the NS topology is derived from the cluster config. It is a no-op when no
// default_domain or S3 bucket is configured, and never overwrites an existing
// zone.
func BootstrapBaseZone(configPath string, cluster *config.ClusterConfig) error {
	serverCfg, err := nsconfig.LoadServerConfig(configPath)
	if err != nil {
		return fmt.Errorf("load northstar config: %w", err)
	}

	domain := strings.TrimSpace(serverCfg.DefaultDomain)
	if domain == "" {
		slog.Debug("northstar bootstrap: no default_domain set, skipping base zone seed")
		return nil
	}
	if serverCfg.S3.Bucket == "" {
		slog.Debug("northstar bootstrap: filesystem mode, skipping base zone seed")
		return nil
	}

	sysCreds := cluster.Nodes[cluster.Node].Predastore
	if sysCreds.AccessKey == "" || sysCreds.SecretKey == "" {
		return fmt.Errorf("missing system predastore credentials for base zone seed")
	}

	// Reuse northstar.toml's endpoint/bucket but with the system (read-write)
	// credentials rather than the daemon's read-only key.
	s3cfg := &nsconfig.S3Config{
		Endpoint:  serverCfg.S3.Endpoint,
		Region:    serverCfg.S3.Region,
		Bucket:    serverCfg.S3.Bucket,
		AccessKey: sysCreds.AccessKey,
		SecretKey: sysCreds.SecretKey,
		Insecure:  serverCfg.S3.Insecure,
	}

	nameservers := buildNameserverSeeds(cluster)
	slog.Info("northstar bootstrap: ensuring base zone",
		"domain", domain, "nameservers", len(nameservers), "multi_node", len(nameservers) > 1)

	created, err := nsconfig.EnsureBaseZone(s3cfg, nsconfig.BaseZoneSeed{
		Domain:      domain,
		Nameservers: nameservers,
		TXT:         []string{baseZoneTXT},
	})
	if err != nil {
		return err
	}
	if created {
		slog.Info("northstar bootstrap: base zone created", "domain", domain)
	} else {
		slog.Info("northstar bootstrap: base zone already present", "domain", domain)
	}
	return nil
}

// buildNameserverSeeds derives one nameserver (nsN → node IP) per cluster node
// that runs northstar, ordered deterministically. Falls back to the local node
// when no node advertises a northstar config (single-node / dev).
func buildNameserverSeeds(cluster *config.ClusterConfig) []nsconfig.NameserverSeed {
	var names []string
	for name, node := range cluster.Nodes {
		if node.Northstar.ConfigPath != "" {
			names = append(names, name)
		}
	}
	sort.Strings(names)
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
