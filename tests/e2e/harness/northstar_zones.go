//go:build e2e

package harness

import (
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	nsconfig "github.com/mulgadc/northstar/pkg/config"
	"github.com/mulgadc/spinifex/spinifex/bootstrap/config"
	"github.com/mulgadc/spinifex/spinifex/domains/dns"
)

const northstarServerConfigPath = "/etc/spinifex/northstar/northstar.toml"

// DNSTargets returns the nodes whose northstar the harness can query on :53:
// every cluster node in multinode mode, else the single local node.
func DNSTargets(env *Env) []Node {
	if env.Mode == ModeMultinode {
		if cluster, err := ClusterFromEnv(); err == nil && len(cluster.Nodes) > 0 {
			return cluster.Nodes
		}
	}
	return []Node{{Index: 1, Name: "local", Addr: localNorthstarAddr(env)}}
}

// localNorthstarAddr is where the local node's northstar answers :53. It binds
// the node's WAN address there, not loopback, so the gateway's dial host is
// only the fallback for a config the harness cannot read.
func localNorthstarAddr(env *Env) string {
	if env.ConfigDir != "" {
		if cc, err := loadClusterConfig(filepath.Join(env.ConfigDir, "spinifex.toml")); err == nil {
			if ips := dns.ResolverNameserverIPs(cc); len(ips) > 0 {
				return ips[0]
			}
		}
	}
	return gatewayDialHost(env)
}

// NorthstarZoneStore returns write credentials for the bucket northstar loads
// zones from: the bucket and region from northstar.toml and the node's system
// predastore key, which is what the control-plane zone writer uses. The
// endpoint host is replaced by the node's address so it is reachable from the
// runner; certificate verification is skipped, as elsewhere in the harness.
func NorthstarZoneStore(t *testing.T, env *Env, node Node) *nsconfig.S3Config {
	t.Helper()
	host := node.Addr
	if env.Mode != ModeMultinode {
		host = ""
	}

	body, err := readNodeFile(host, northstarServerConfigPath)
	if err != nil {
		t.Fatalf("read %s on %s: %v", northstarServerConfigPath, hostLabel(host), err)
	}
	path := filepath.Join(t.TempDir(), "northstar.toml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("stage northstar.toml: %v", err)
	}
	server, err := nsconfig.LoadServerConfig(path)
	if err != nil {
		t.Fatalf("parse northstar.toml from %s: %v", hostLabel(host), err)
	}
	if server.S3.Bucket == "" {
		t.Skipf("northstar on %s serves zones from %q, not S3", hostLabel(host), server.ZoneDir)
	}

	cc := nodeClusterConfig(t, host)
	creds := cc.Nodes[cc.Node].Predastore
	if creds.AccessKey == "" || creds.SecretKey == "" {
		t.Fatalf("no system predastore credentials in spinifex.toml on %s", hostLabel(host))
	}

	endpoint := server.S3.Endpoint
	if env.Mode == ModeMultinode {
		endpoint = withHost(endpoint, node.Addr)
	}
	return &nsconfig.S3Config{
		Endpoint:  endpoint,
		Region:    server.S3.Region,
		Bucket:    server.S3.Bucket,
		AccessKey: creds.AccessKey,
		SecretKey: creds.SecretKey,
		Insecure:  true,
	}
}

// WriteTestZone stores a minimal zone for domain whose apex A record is addr.
func WriteTestZone(s3cfg *nsconfig.S3Config, domain, addr string) error {
	zone := nsconfig.NewZoneConfig(nsconfig.BaseZoneSeed{
		Domain:      domain,
		Nameservers: []nsconfig.NameserverSeed{{Host: "ns1", IP: addr}},
	})
	zone.UpsertRecord("", nsconfig.TypeA, nsconfig.ClassIN, addr, 0)
	body, err := nsconfig.RenderZone(zone)
	if err != nil {
		return fmt.Errorf("render %s: %w", domain, err)
	}
	return nsconfig.WriteZoneFile(s3cfg, domain, body)
}

// DeleteTestZone removes domain's zone object.
func DeleteTestZone(ctx context.Context, s3cfg *nsconfig.S3Config, domain string) error {
	_, err := zoneStoreClient(s3cfg).DeleteObject(ctx, &s3.DeleteObjectInput{
		Bucket: aws.String(s3cfg.Bucket),
		Key:    aws.String(strings.TrimSuffix(domain, ".") + ".toml"),
	})
	return err
}

func zoneStoreClient(s3cfg *nsconfig.S3Config) *s3.Client {
	endpoint := s3cfg.Endpoint
	if !strings.Contains(endpoint, "://") {
		endpoint = "https://" + endpoint
	}
	return s3.New(s3.Options{
		Region:       s3cfg.Region,
		BaseEndpoint: aws.String(endpoint),
		UsePathStyle: true,
		Credentials:  credentials.NewStaticCredentialsProvider(s3cfg.AccessKey, s3cfg.SecretKey, ""),
		HTTPClient:   insecureHTTPClient(30 * time.Second),
	})
}

// withHost replaces the host of an endpoint, keeping its scheme and port.
func withHost(endpoint, host string) string {
	scheme := ""
	rest := endpoint
	if i := strings.Index(endpoint, "://"); i >= 0 {
		scheme, rest = endpoint[:i+3], endpoint[i+3:]
	}
	if _, port, err := net.SplitHostPort(rest); err == nil {
		return scheme + net.JoinHostPort(host, port)
	}
	return scheme + host
}

func nodeClusterConfig(t *testing.T, host string) *config.ClusterConfig {
	t.Helper()
	body, err := readNodeFile(host, "/etc/spinifex/spinifex.toml")
	if err != nil {
		t.Fatalf("read spinifex.toml on %s: %v", hostLabel(host), err)
	}
	cc, err := loadClusterConfigBytes([]byte(body))
	if err != nil {
		t.Fatalf("decode spinifex.toml from %s: %v", hostLabel(host), err)
	}
	return cc
}
