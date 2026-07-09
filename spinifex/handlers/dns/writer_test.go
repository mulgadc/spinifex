package dns

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

// fakeS3 is a minimal mutable path-style S3 endpoint (HEAD/PUT/GET) backing the
// writer's read-modify-write.
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

// newTestWriter builds an enabled Writer backed by fakeS3 with the base zone
// pre-seeded (as the bootstrap would leave it).
func newTestWriter(t *testing.T) (*Writer, map[string]string) {
	t.Helper()
	endpoint, objects := fakeS3(t, "northstar")
	// Pre-seed the base zone so upserts to spx3.net read-modify an existing object.
	objects["spx3.net.toml"] = `version = 1.0
[domain]
domain = "spx3.net"
active = true
soa = "ns1.spx3.net."
[defaults]
ttl = 300
type = 1
class = 1
[[records]]
domain = ""
type = 2
address = "ns1.spx3.net."
`
	tomlBody := fmt.Sprintf(`listen = "0.0.0.0:5300"
default_domain = "spx3.net"
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

	cfg := &config.Config{
		Host:       "10.11.12.1",
		Predastore: config.PredastoreConfig{AccessKey: "SYSTEM", SecretKey: "SYSTEMSECRET"},
		Northstar:  config.NorthstarConfig{ConfigPath: configPath},
	}
	w := NewWriter(cfg, nil)
	require.True(t, w.Enabled(), "writer should be enabled")
	return w, objects
}

func TestWriterUpsertPublicAndPrivate(t *testing.T) {
	w, objects := newTestWriter(t)

	changes := EC2Changes(ActionUpsert, "ap-southeast-2", "spx3.net", "", "1.2.3.4", "172.31.26.216")
	res, err := w.ApplyBatch(&ChangeBatch{Changes: changes})
	require.NoError(t, err)
	assert.Equal(t, 2, res.Applied)

	// Public record landed in the existing base zone.
	base := objects["spx3.net.toml"]
	assert.Contains(t, base, `domain = "ec2-1-2-3-4.ap-southeast-2.compute."`)
	assert.Contains(t, base, `address = "1.2.3.4"`)

	// Private record materialised the compute.internal zone on demand.
	priv, ok := objects["compute.internal.toml"]
	require.True(t, ok, "compute.internal zone created")
	assert.Contains(t, priv, `domain = "ip-172-31-26-216.ap-southeast-2."`)
	assert.Contains(t, priv, `address = "172.31.26.216"`)
}

func TestWriterUpsertIsIdempotentAndDeletes(t *testing.T) {
	w, objects := newTestWriter(t)
	changes := EC2Changes(ActionUpsert, "ap-southeast-2", "spx3.net", "", "1.2.3.4", "172.31.26.216")

	_, err := w.ApplyBatch(&ChangeBatch{Changes: changes})
	require.NoError(t, err)
	first := objects["spx3.net.toml"]

	// Re-applying identical upserts must not change the zone body.
	_, err = w.ApplyBatch(&ChangeBatch{Changes: changes})
	require.NoError(t, err)
	assert.Equal(t, first, objects["spx3.net.toml"])

	// Delete withdraws both records.
	del := EC2Changes(ActionDelete, "ap-southeast-2", "spx3.net", "", "1.2.3.4", "172.31.26.216")
	_, err = w.ApplyBatch(&ChangeBatch{Changes: del})
	require.NoError(t, err)
	assert.NotContains(t, objects["spx3.net.toml"], "ec2-1-2-3-4")
	assert.NotContains(t, objects["compute.internal.toml"], "ip-172-31-26-216")
}

func TestWriterDeleteMissingZoneNoop(t *testing.T) {
	w, objects := newTestWriter(t)
	// Delete a private record before any private zone exists → no zone created.
	del := EC2Changes(ActionDelete, "ap-southeast-2", "spx3.net", "", "", "172.31.26.216")
	res, err := w.ApplyBatch(&ChangeBatch{Changes: del})
	require.NoError(t, err)
	assert.Equal(t, 1, res.Applied)
	_, ok := objects["compute.internal.toml"]
	assert.False(t, ok, "no zone materialised for a delete-only batch")
}

func TestWriterRejectsUpsertOverRecordQuota(t *testing.T) {
	w, objects := newTestWriter(t)
	w.quotas.RecordsPerHostedZone = 1 // base zone already holds the apex NS record

	changes := []Change{{Action: ActionUpsert, Zone: "spx3.net", Name: "host.spx3.net", Type: "A", Value: "1.2.3.4"}}
	_, err := w.ApplyBatch(&ChangeBatch{Changes: changes})
	require.Error(t, err, "adding a new record set past the quota must be rejected")
	assert.Contains(t, err.Error(), "record quota")
	assert.NotContains(t, objects["spx3.net.toml"], "1.2.3.4", "the zone must not grow past the quota")
}

func TestWriterReplacingRecordSetNotQuotaLimited(t *testing.T) {
	w, objects := newTestWriter(t)

	add := []Change{{Action: ActionUpsert, Zone: "spx3.net", Name: "host.spx3.net", Type: "A", Value: "1.2.3.4"}}
	_, err := w.ApplyBatch(&ChangeBatch{Changes: add})
	require.NoError(t, err) // spx3.net now holds apex-NS + host = 2 record sets

	// Pin the quota at the current size: replacing an existing record set (same
	// label+type, new value) must still succeed — only new sets are capped.
	w.quotas.RecordsPerHostedZone = 2
	replace := []Change{{Action: ActionUpsert, Zone: "spx3.net", Name: "host.spx3.net", Type: "A", Value: "5.6.7.8"}}
	_, err = w.ApplyBatch(&ChangeBatch{Changes: replace})
	require.NoError(t, err, "replacing an existing record set must not hit the record quota")
	assert.Contains(t, objects["spx3.net.toml"], `address = "5.6.7.8"`)
}

func TestWriterDisabledWithoutNorthstar(t *testing.T) {
	w := NewWriter(&config.Config{}, nil)
	assert.False(t, w.Enabled())
	_, err := w.ApplyBatch(&ChangeBatch{Changes: []Change{{Action: ActionUpsert}}})
	require.Error(t, err)
}

func TestResolveBaseDomain(t *testing.T) {
	w, _ := newTestWriter(t)
	assert.Equal(t, "spx3.net", w.baseDomain)
	assert.Empty(t, ResolveBaseDomain(&config.Config{}))
}

// The resolvers prefer the non-secret cluster-config domains so confined
// services need not read the 0600 northstar.toml.
func TestResolveDomainsPreferClusterConfig(t *testing.T) {
	cfg := &config.Config{Northstar: config.NorthstarConfig{
		DefaultDomain:  "example.net",
		InternalDomain: "vpc.internal",
	}}
	assert.Equal(t, "example.net", ResolveBaseDomain(cfg))
	assert.Equal(t, "vpc.internal", ResolveInternalDomain(cfg))
}
