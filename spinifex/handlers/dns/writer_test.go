package dns

import (
	"crypto/x509"
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

	nsconfig "github.com/mulgadc/northstar/pkg/config"
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
	return newTestWriterWithQuota(t, true, DefaultRecordsPerHostedZone)
}

func newTestWriterWithQuota(t *testing.T, quotaEnabled bool, recordsPerHostedZone int) (*Writer, map[string]string) {
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

[quotas]
enabled = %t
records_per_hosted_zone = %d
`, endpoint, quotaEnabled, recordsPerHostedZone)
	configPath := filepath.Join(t.TempDir(), "northstar.toml")
	require.NoError(t, os.WriteFile(configPath, []byte(tomlBody), 0o600))

	cfg := &config.Config{
		Host:       "10.11.12.1",
		Predastore: config.PredastoreConfig{AccessKey: "SYSTEM", SecretKey: "SYSTEMSECRET"},
		Northstar:  config.NorthstarConfig{ConfigPath: configPath},
	}
	w := NewWriter(cfg, nil, nil)
	require.True(t, w.Enabled(), "writer should be enabled")
	return w, objects
}

// newMultiNodeTestWriter builds an enabled Writer for a two-node cluster against
// an empty bucket, so zone materialisation runs instead of read-modify-write.
func newMultiNodeTestWriter(t *testing.T) (*Writer, map[string]string) {
	t.Helper()
	endpoint, objects := fakeS3(t, "northstar")
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

	local := config.Config{
		Node:        "node1",
		AdvertiseIP: "10.0.0.1",
		Predastore:  config.PredastoreConfig{AccessKey: "SYSTEM", SecretKey: "SYSTEMSECRET"},
		Northstar:   config.NorthstarConfig{ConfigPath: configPath},
	}
	cluster := &config.ClusterConfig{
		Node: "node1",
		Nodes: map[string]config.Config{
			"node1": local,
			"node2": {Node: "node2", AdvertiseIP: "10.0.0.2", Northstar: config.NorthstarConfig{ConfigPath: configPath}},
		},
	}
	w := NewWriter(&local, cluster, nil)
	require.True(t, w.Enabled(), "writer should be enabled")
	return w, objects
}

func TestZoneS3ConfigRejectsTOMLInsecure(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(server.Close)

	tomlBody := fmt.Sprintf(`default_domain = "spx3.net"
[s3]
endpoint = %q
bucket = "northstar"
region = "us-east-1"
access_key = "READONLY"
secret_key = "READONLY"
insecure = true
`, server.URL)
	configPath := filepath.Join(t.TempDir(), "northstar.toml")
	require.NoError(t, os.WriteFile(configPath, []byte(tomlBody), 0o600))

	cfg := &config.Config{
		Predastore: config.PredastoreConfig{AccessKey: "SYSTEM", SecretKey: "SYSTEMSECRET"},
		Northstar:  config.NorthstarConfig{ConfigPath: configPath},
	}
	zoneCfg, ok := zoneS3Config(cfg)
	require.True(t, ok)
	require.False(t, zoneCfg.s3.Insecure)

	err := nsconfig.WriteZoneFile(zoneCfg.s3, "spx3.net", []byte("zone"))
	require.Error(t, err)

	var unknownAuthority x509.UnknownAuthorityError
	require.ErrorAs(t, err, &unknownAuthority)
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

func TestRecordType(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want uint16
	}{
		{name: "A", in: "A", want: nsconfig.TypeA},
		{name: "lowercase A", in: "a", want: nsconfig.TypeA},
		{name: "NS", in: "NS", want: nsconfig.TypeNS},
		{name: "TXT", in: "TXT", want: nsconfig.TypeTXT},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := recordType(tt.in)
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestWriterRejectsUnsupportedRecordType(t *testing.T) {
	tests := []struct {
		name  string
		rtype string
	}{
		{name: "empty", rtype: ""},
		{name: "AAAA", rtype: "AAAA"},
		{name: "misspelled", rtype: "AA"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w, objects := newTestWriter(t)
			before := objects["spx3.net.toml"]
			change := Change{
				Action: ActionUpsert,
				Zone:   "spx3.net",
				Name:   "host.spx3.net",
				Type:   tt.rtype,
				Value:  "2001:db8::1",
			}

			_, err := w.ApplyBatch(&ChangeBatch{Changes: []Change{change}})
			require.Error(t, err)
			assert.Contains(t, err.Error(), "unsupported DNS record type")
			assert.Equal(t, before, objects["spx3.net.toml"], "invalid input must not mutate the zone")
		})
	}
}

func TestWriterQuotaDisabledAllowsGrowth(t *testing.T) {
	w, objects := newTestWriterWithQuota(t, false, 1)

	changes := []Change{{Action: ActionUpsert, Zone: "spx3.net", Name: "host.spx3.net", Type: "A", Value: "1.2.3.4"}}
	_, err := w.ApplyBatch(&ChangeBatch{Changes: changes})
	require.NoError(t, err)
	assert.Contains(t, objects["spx3.net.toml"], "1.2.3.4", "a disabled quota must not cap zone growth")
}

func TestWriterEnabledNonPositiveQuotaDefaultsToAWSLimit(t *testing.T) {
	tests := []struct {
		name  string
		limit int
	}{
		{name: "unset", limit: 0},
		{name: "negative", limit: -1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w, objects := newTestWriterWithQuota(t, true, tt.limit)
			cfg := nsconfig.NewZoneConfig(nsconfig.BaseZoneSeed{Domain: "spx3.net"})
			cfg.Records = make([]nsconfig.Records, DefaultRecordsPerHostedZone)
			for i := range cfg.Records {
				cfg.Records[i] = nsconfig.Records{
					Domain:  fmt.Sprintf("host-%d", i),
					Type:    nsconfig.TypeA,
					Class:   nsconfig.ClassIN,
					Address: "1.2.3.4",
				}
			}
			body, err := nsconfig.RenderZone(cfg)
			require.NoError(t, err)
			objects["spx3.net.toml"] = string(body)

			changes := []Change{{Action: ActionUpsert, Zone: "spx3.net", Name: "overflow.spx3.net", Type: "A", Value: "5.6.7.8"}}
			_, err = w.ApplyBatch(&ChangeBatch{Changes: changes})
			require.Error(t, err, "the AWS default must reject record set 10,001")
			assert.Contains(t, err.Error(), "record quota (10000)")
		})
	}
}

func TestWriterRejectsUpsertOverRecordQuota(t *testing.T) {
	w, objects := newTestWriterWithQuota(t, true, 1) // base zone already holds the apex NS record

	changes := []Change{{Action: ActionUpsert, Zone: "spx3.net", Name: "host.spx3.net", Type: "A", Value: "1.2.3.4"}}
	_, err := w.ApplyBatch(&ChangeBatch{Changes: changes})
	require.Error(t, err, "adding a new record set past the quota must be rejected")
	assert.Contains(t, err.Error(), "record quota")
	assert.NotContains(t, objects["spx3.net.toml"], "1.2.3.4", "the zone must not grow past the quota")
}

func TestWriterReplacingRecordSetNotQuotaLimited(t *testing.T) {
	w, objects := newTestWriterWithQuota(t, true, 2)

	add := []Change{{Action: ActionUpsert, Zone: "spx3.net", Name: "host.spx3.net", Type: "A", Value: "1.2.3.4"}}
	_, err := w.ApplyBatch(&ChangeBatch{Changes: add})
	require.NoError(t, err) // spx3.net now holds apex-NS + host = 2 record sets

	// Replacing an existing record set (same label+type, new value) must still
	// succeed at the quota because only new sets are capped.
	replace := []Change{{Action: ActionUpsert, Zone: "spx3.net", Name: "host.spx3.net", Type: "A", Value: "5.6.7.8"}}
	_, err = w.ApplyBatch(&ChangeBatch{Changes: replace})
	require.NoError(t, err, "replacing an existing record set must not hit the record quota")
	assert.Contains(t, objects["spx3.net.toml"], `address = "5.6.7.8"`)
}

// A zone materialised on demand must carry every northstar node, not just the
// node that happened to consume the change. Both this path and BootstrapBaseZone
// create-if-absent and never overwrite, so a single degenerate ns1 written here
// first would pin the zone's NS topology to one node for the life of the object.
// Asserted on a multi-node cluster: on single-node the two derivations coincide.
func TestWriterMaterialisesZoneWithClusterNameservers(t *testing.T) {
	w, objects := newMultiNodeTestWriter(t)

	_, err := w.ApplyBatch(&ChangeBatch{Changes: []Change{
		{Action: ActionUpsert, Zone: "compute.internal", Name: "i-123.compute.internal", Type: "A", Value: "10.20.0.5"},
	}})
	require.NoError(t, err)

	body := objects["compute.internal.toml"]
	require.NotEmpty(t, body, "a missing zone with an upsert should be materialised")

	// Both nodes' NS records and glue, exactly as BootstrapBaseZone would seed them.
	assert.Contains(t, body, `address = "ns1.compute.internal."`)
	assert.Contains(t, body, `address = "ns2.compute.internal."`)
	assert.Contains(t, body, `address = "10.0.0.1"`)
	assert.Contains(t, body, `address = "10.0.0.2"`)
	assert.Contains(t, body, `soa = "ns1.compute.internal."`)
}

func TestWriterDisabledWithoutNorthstar(t *testing.T) {
	w := NewWriter(&config.Config{}, nil, nil)
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
