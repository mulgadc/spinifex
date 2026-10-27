package dns_test

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	testutil "github.com/mulgadc/spinifex/internal/testkit"
	"github.com/mulgadc/spinifex/spinifex/bootstrap/config"
	"github.com/mulgadc/spinifex/spinifex/domains/dns"
)

// zoneBucket is a path-style S3 endpoint holding whole objects.
func zoneBucket(t *testing.T) (endpoint string, get func(key string) (string, bool)) {
	t.Helper()
	var mu sync.Mutex
	objects := map[string]string{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := strings.TrimPrefix(r.URL.Path, "/northstar/")
		mu.Lock()
		defer mu.Unlock()
		switch r.Method {
		case http.MethodPut:
			body, _ := io.ReadAll(r.Body)
			objects[key] = string(body)
		case http.MethodDelete:
			delete(objects, key)
			w.WriteHeader(http.StatusNoContent)
		default:
			http.Error(w, "unsupported", http.StatusMethodNotAllowed)
		}
	}))
	t.Cleanup(srv.Close)
	return srv.URL, func(key string) (string, bool) {
		mu.Lock()
		defer mu.Unlock()
		v, ok := objects[key]
		return v, ok
	}
}

func publisherConfig(t *testing.T, endpoint string) *config.Config {
	t.Helper()
	path := filepath.Join(t.TempDir(), "northstar.toml")
	body := fmt.Sprintf("listen = \"0.0.0.0:5300\"\ndefault_domain = \"spx3.net\"\n[s3]\nendpoint = %q\nbucket = \"northstar\"\nregion = \"us-east-1\"\naccess_key = \"RO\"\nsecret_key = \"RO\"\n", endpoint)
	require.NoError(t, os.WriteFile(path, []byte(body), 0o600))
	return &config.Config{
		Predastore: config.PredastoreConfig{AccessKey: "SYSTEM", SecretKey: "SYSTEMSECRET"},
		Northstar:  config.NorthstarConfig{ConfigPath: path},
	}
}

func cluster(nodes ...string) *config.ClusterConfig {
	c := &config.ClusterConfig{Node: nodes[0], Nodes: map[string]config.Config{}}
	for _, n := range nodes {
		c.Nodes[n] = config.Config{Northstar: config.NorthstarConfig{ConfigPath: "/etc/spinifex/northstar.toml"}}
	}
	return c
}

func TestZonePublisherWritesAndDeletesZoneObjects(t *testing.T) {
	endpoint, get := zoneBucket(t)
	_, nc := testutil.StartTestNATS(t)
	pub, ok := dns.NewZonePublisher(publisherConfig(t, endpoint), func() *config.ClusterConfig { return cluster("node1") }, nc)
	require.True(t, ok)

	require.NoError(t, pub.PutZone(context.Background(), "Example.com", []byte("body")))
	body, ok := get("example.com.toml")
	require.True(t, ok)
	assert.Equal(t, "body", body)

	require.NoError(t, pub.DeleteZone(context.Background(), "example.com"))
	_, ok = get("example.com.toml")
	assert.False(t, ok)
}

func TestZonePublisherNeedsTheZoneStore(t *testing.T) {
	_, nc := testutil.StartTestNATS(t)
	_, ok := dns.NewZonePublisher(&config.Config{}, func() *config.ClusterConfig { return nil }, nc)
	assert.False(t, ok)
}

// A reload is complete only when every northstar node has answered; a failed
// node reports a version no digest matches, and a node answering twice does not
// stand in for one that never answered.
func TestZonePublisherReloadCountsEveryNorthstarNode(t *testing.T) {
	endpoint, _ := zoneBucket(t)
	nodes := cluster("node1", "node2", "node3")
	cases := []struct {
		name     string
		acks     []dns.ZoneReloadAck
		served   []string
		complete bool
	}{
		{"one failed, one silent", []dns.ZoneReloadAck{{Node: "node1", ETag: "v1"}, {Node: "node2", Error: "s3 down"}}, []string{"v1", "error: s3 down"}, false},
		{"one node twice", []dns.ZoneReloadAck{{Node: "node1", ETag: "v1"}, {Node: "node1", ETag: "v1"}}, []string{"v1", "v1"}, false},
		{"every node", []dns.ZoneReloadAck{{Node: "node1", ETag: "v1"}, {Node: "node2", ETag: "v1"}, {Node: "node3", ETag: "v1"}}, []string{"v1", "v1", "v1"}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, nc := testutil.StartTestNATS(t)
			pub, ok := dns.NewZonePublisher(publisherConfig(t, endpoint), func() *config.ClusterConfig { return nodes }, nc)
			require.True(t, ok)
			ackAs(t, nc, func(zone string) []dns.ZoneReloadAck {
				acks := slices.Clone(tc.acks)
				for i := range acks {
					acks[i].Zone = zone
				}
				return acks
			})

			served, complete, err := pub.Reload(context.Background(), "example.com")
			require.NoError(t, err)
			assert.Equal(t, tc.complete, complete)
			assert.ElementsMatch(t, tc.served, served)
		})
	}
}

func TestNorthstarServerCount(t *testing.T) {
	assert.Equal(t, 1, dns.NorthstarServerCount(nil))
	assert.Equal(t, 1, dns.NorthstarServerCount(&config.ClusterConfig{Nodes: map[string]config.Config{"node1": {}}}))
	assert.Equal(t, 3, dns.NorthstarServerCount(cluster("node1", "node2", "node3")))
}
