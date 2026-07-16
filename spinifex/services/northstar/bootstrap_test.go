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
	"time"

	"github.com/mulgadc/spinifex/spinifex/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

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

	// The AWS-parity private zone is seeded alongside the base zone.
	require.Contains(t, objects, "compute.internal.toml")
	internal := objects["compute.internal.toml"]
	assert.Contains(t, internal, `domain = "compute.internal"`)
	assert.Contains(t, internal, "10.11.12.1") // shares the NS glue topology

	// Second start is idempotent — does not rewrite either zone.
	require.NoError(t, BootstrapBaseZone(configPath, cluster))
	assert.Equal(t, body, objects["spx3.net.toml"])
	assert.Equal(t, internal, objects["compute.internal.toml"])
}

// flakyS3 refuses the first failUntil requests (simulating predastore not yet
// listening) then behaves like fakeS3, to exercise the bootstrap retry loop.
func flakyS3(t *testing.T, bucket string, failUntil int) (endpoint string, objects map[string]string) {
	t.Helper()
	var mu sync.Mutex
	objects = map[string]string{}
	var calls int

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		calls++
		if calls <= failUntil {
			http.Error(w, "unavailable", http.StatusServiceUnavailable)
			return
		}
		key := strings.TrimPrefix(r.URL.Path, "/"+bucket+"/")
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
		default:
			http.Error(w, "unsupported", http.StatusMethodNotAllowed)
		}
	}))
	t.Cleanup(srv.Close)
	return srv.URL, objects
}

func TestBootstrapBaseZoneRetries(t *testing.T) {
	prev := bootstrapRetryDelay
	bootstrapRetryDelay = time.Millisecond
	t.Cleanup(func() { bootstrapRetryDelay = prev })

	// Fail the first two S3 requests, then succeed — the retry must seed.
	endpoint, objects := flakyS3(t, "northstar", 2)
	tomlBody := fmt.Sprintf(`listen = "0.0.0.0:5300"
default_domain = "spx3.net"
[s3]
endpoint = %q
bucket = "northstar"
region = "us-east-1"
access_key = "READONLY"
secret_key = "READONLY"
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

	require.NoError(t, BootstrapBaseZone(configPath, cluster))
	require.Contains(t, objects, "spx3.net.toml")
}

func TestNodeNames(t *testing.T) {
	cluster := &config.ClusterConfig{Nodes: map[string]config.Config{"b": {}, "a": {}, "c": {}}}
	assert.Equal(t, []string{"a", "b", "c"}, nodeNames(cluster))
}

func TestBootstrapBaseZoneUnknownNode(t *testing.T) {
	endpoint, _ := fakeS3(t, "northstar")
	tomlBody := fmt.Sprintf(`default_domain = "spx3.net"
[s3]
endpoint = %q
bucket = "northstar"
access_key = "READONLY"
secret_key = "READONLY"
`, endpoint)
	configPath := filepath.Join(t.TempDir(), "northstar.toml")
	require.NoError(t, os.WriteFile(configPath, []byte(tomlBody), 0o600))

	cluster := &config.ClusterConfig{Node: "missing", Nodes: map[string]config.Config{"node1": {}}}
	err := BootstrapBaseZone(configPath, cluster)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not found in cluster config")
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
