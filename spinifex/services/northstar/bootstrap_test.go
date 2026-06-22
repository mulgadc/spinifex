package northstar

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/mulgadc/spinifex/spinifex/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNameserverIP(t *testing.T) {
	assert.Equal(t, "10.11.12.1", nameserverIP(config.Config{AdvertiseIP: "10.11.12.1"}))
	assert.Equal(t, "10.11.12.2", nameserverIP(config.Config{Host: "10.11.12.2:8443"}))
	assert.Equal(t, "10.0.0.9", nameserverIP(config.Config{AdvertiseIP: "10.0.0.9", Host: "10.0.0.1"}))
	assert.Equal(t, "127.0.0.1", nameserverIP(config.Config{Host: "0.0.0.0"}))
	assert.Equal(t, "127.0.0.1", nameserverIP(config.Config{}))
}

func TestBuildNameserverSeeds(t *testing.T) {
	// Single node with no northstar config path → fall back to the local node.
	single := &config.ClusterConfig{
		Node:  "node1",
		Nodes: map[string]config.Config{"node1": {Host: "0.0.0.0"}},
	}
	seeds := buildNameserverSeeds(single)
	require.Len(t, seeds, 1)
	assert.Equal(t, "ns1", seeds[0].Host)
	assert.Equal(t, "127.0.0.1", seeds[0].IP)

	// Multi-node: one nameserver per node that advertises a northstar config,
	// ordered deterministically.
	multi := &config.ClusterConfig{
		Node: "node1",
		Nodes: map[string]config.Config{
			"node2": {AdvertiseIP: "10.0.0.2", Northstar: config.NorthstarConfig{ConfigPath: "/etc/n.toml"}},
			"node1": {AdvertiseIP: "10.0.0.1", Northstar: config.NorthstarConfig{ConfigPath: "/etc/n.toml"}},
			"node3": {AdvertiseIP: "10.0.0.3"}, // no northstar → excluded
		},
	}
	seeds = buildNameserverSeeds(multi)
	require.Len(t, seeds, 2)
	assert.Equal(t, "ns1", seeds[0].Host)
	assert.Equal(t, "10.0.0.1", seeds[0].IP)
	assert.Equal(t, "ns2", seeds[1].Host)
	assert.Equal(t, "10.0.0.2", seeds[1].IP)
}

// fakeS3 is a minimal mutable path-style S3 endpoint (HEAD/PUT/GET) for the
// bootstrap happy-path test.
func fakeS3(t *testing.T, bucket string) (endpoint string, objects map[string]string) {
	t.Helper()
	var mu sync.Mutex
	objects = map[string]string{}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := strings.TrimPrefix(r.URL.Path, "/"+bucket+"/")
		mu.Lock()
		defer mu.Unlock()
		switch r.Method {
		case http.MethodHead:
			if _, ok := objects[key]; ok {
				w.WriteHeader(http.StatusOK)
			} else {
				w.WriteHeader(http.StatusNotFound)
			}
		case http.MethodPut:
			body, _ := io.ReadAll(r.Body)
			objects[key] = string(body)
			w.WriteHeader(http.StatusOK)
		case http.MethodGet:
			body, ok := objects[key]
			if !ok {
				http.Error(w, "not found", http.StatusNotFound)
				return
			}
			w.Header().Set("Content-Length", strconv.Itoa(len(body)))
			_, _ = w.Write([]byte(body))
		default:
			http.Error(w, "unsupported", http.StatusMethodNotAllowed)
		}
	}))
	t.Cleanup(srv.Close)
	return srv.URL, objects
}

func TestBootstrapBaseZone(t *testing.T) {
	endpoint, objects := fakeS3(t, "northstar")

	tomlBody := fmt.Sprintf(`listen = "0.0.0.0:5300"
default_domain = "spx3.net"
sync_interval = 30
[s3]
endpoint = %q
bucket = "northstar"
region = "us-east-1"
access_key = "READONLY"
secret_key = "READONLY"
insecure = true
`, endpoint)
	configPath := filepath.Join(t.TempDir(), "northstar.toml")
	require.NoError(t, os.WriteFile(configPath, []byte(tomlBody), 0o600))

	cluster := &config.ClusterConfig{
		Node: "node1",
		Nodes: map[string]config.Config{
			"node1": {
				Host:       "10.11.12.1",
				Predastore: config.PredastoreConfig{AccessKey: "SYSTEM", SecretKey: "SYSTEMSECRET"},
				Northstar:  config.NorthstarConfig{ConfigPath: configPath},
			},
		},
	}

	// First start creates the base zone.
	require.NoError(t, BootstrapBaseZone(configPath, cluster))
	require.Contains(t, objects, "spx3.net.toml")
	body := objects["spx3.net.toml"]
	assert.Contains(t, body, `domain = "spx3.net"`)
	assert.Contains(t, body, "10.11.12.1") // glue A for the nameserver
	assert.Contains(t, body, baseZoneTXT)  // base TXT marker
	assert.Contains(t, body, "type = 2")   // NS record

	// Second start is idempotent — does not rewrite the zone.
	require.NoError(t, BootstrapBaseZone(configPath, cluster))
	assert.Equal(t, body, objects["spx3.net.toml"])
}

func TestBootstrapBaseZoneNoDomain(t *testing.T) {
	tomlBody := `listen = "0.0.0.0:5300"
zone_dir = "/tmp/zones"
sync_interval = 30
`
	configPath := filepath.Join(t.TempDir(), "northstar.toml")
	require.NoError(t, os.WriteFile(configPath, []byte(tomlBody), 0o600))

	cluster := &config.ClusterConfig{Node: "node1", Nodes: map[string]config.Config{"node1": {}}}
	// No default_domain → no-op, no error.
	require.NoError(t, BootstrapBaseZone(configPath, cluster))
}
