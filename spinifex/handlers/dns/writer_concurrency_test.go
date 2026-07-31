package dns

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	nsconfig "github.com/mulgadc/northstar/pkg/config"
	"github.com/mulgadc/spinifex/spinifex/config"
	"github.com/mulgadc/spinifex/spinifex/testutil"
	"github.com/nats-io/nats.go"
	toml "github.com/pelletier/go-toml/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// parseZone decodes a stored zone object the same way ReadZoneRaw does, so a
// body that fails here is exactly the body that wedges the writer and northstar.
func parseZone(t *testing.T, body string) nsconfig.ConfigArr {
	t.Helper()
	var cfg nsconfig.ConfigArr
	require.NoError(t, toml.Unmarshal([]byte(body), &cfg))
	return cfg
}

// hasA reports whether the zone holds an A record with the given address.
func hasA(cfg nsconfig.ConfigArr, address string) bool {
	for _, rec := range cfg.Records {
		if rec.Type == nsconfig.TypeA && rec.Address == address {
			return true
		}
	}
	return false
}

// clusterWriters builds n Writers that share one fakeS3 bucket and one NATS
// connection, standing in for n daemons in a queue group. The base zone is
// pre-seeded so every writer read-modify-writes an existing object, which is the
// racing path.
func clusterWriters(t *testing.T, nc *nats.Conn, n int) ([]*Writer, map[string]string) {
	t.Helper()
	endpoint, objects := fakeS3(t, "northstar")
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
`, endpoint)
	configPath := filepath.Join(t.TempDir(), "northstar.toml")
	require.NoError(t, os.WriteFile(configPath, []byte(tomlBody), 0o600))

	writers := make([]*Writer, 0, n)
	for i := range n {
		cfg := &config.Config{
			Node:       fmt.Sprintf("node%d", i+1),
			Predastore: config.PredastoreConfig{AccessKey: "SYSTEM", SecretKey: "SYSTEMSECRET"},
			Northstar:  config.NorthstarConfig{ConfigPath: configPath},
		}
		w := NewWriter(cfg, nil, nc)
		require.True(t, w.Enabled())
		writers = append(writers, w)
	}
	return writers, objects
}

// TestConcurrentWritersDoNotLoseRecords is the regression test for the zone
// read-modify-write race: a NATS queue group load-balances messages rather than
// serialising them, so before the per-zone lock, concurrent daemons overwrote
// each other's records.
//
// fakeS3 serialises whole requests under a mutex, so this reproduces the
// lost-update half of the bug, not the byte-splicing half that needs a real
// non-atomic multi-shard PUT.
func TestConcurrentWritersDoNotLoseRecords(t *testing.T) {
	_, nc, _ := testutil.StartTestJetStream(t)
	shrinkZoneLockWaits(t, 10*time.Second)

	const nodes = 6
	writers, objects := clusterWriters(t, nc, nodes)

	var wg sync.WaitGroup
	errs := make([]error, nodes)
	for i, w := range writers {
		wg.Go(func() {
			_, errs[i] = w.ApplyBatch(&ChangeBatch{Changes: []Change{{
				Action: ActionUpsert,
				Zone:   "spx3.net",
				Name:   fmt.Sprintf("lb-%d.elb.spx3.net", i),
				Type:   "A",
				Value:  fmt.Sprintf("10.200.1.%d", i+10),
			}}})
		})
	}
	wg.Wait()

	for i, err := range errs {
		require.NoError(t, err, "writer %d", i)
	}

	body, ok := objects["spx3.net.toml"]
	require.True(t, ok, "zone object must exist")

	// The object must still parse: an interleaved write is what produced
	// "toml: unterminated basic string" in production.
	cfg := parseZone(t, body)

	// Every writer's record must have survived, not just the last one in.
	for i := range nodes {
		want := fmt.Sprintf("10.200.1.%d", i+10)
		assert.True(t, hasA(cfg, want), "record %s from writer %d was lost", want, i)
	}

	// The pre-seeded NS record must not have been dropped along the way.
	var foundNS bool
	for _, rec := range cfg.Records {
		if rec.Type == nsconfig.TypeNS {
			foundNS = true
		}
	}
	assert.True(t, foundNS, "structural NS record must survive concurrent upserts")
}

// TestConcurrentWritersAcrossZonesDoNotBlock proves the lock is per-zone: two
// writers hitting different zones must both complete without contending.
func TestConcurrentWritersAcrossZonesDoNotBlock(t *testing.T) {
	_, nc, _ := testutil.StartTestJetStream(t)
	// A wait budget this short fails if the two writers serialise on one key.
	shrinkZoneLockWaits(t, 200*time.Millisecond)

	writers, objects := clusterWriters(t, nc, 2)

	var wg sync.WaitGroup
	errs := make([]error, 2)
	zones := []string{"spx3.net", "compute.internal"}
	for i, w := range writers {
		wg.Go(func() {
			_, errs[i] = w.ApplyBatch(&ChangeBatch{Changes: []Change{{
				Action: ActionUpsert,
				Zone:   zones[i],
				Name:   "host." + zones[i],
				Type:   "A",
				Value:  fmt.Sprintf("10.0.0.%d", i+1),
			}}})
		})
	}
	wg.Wait()

	for i, err := range errs {
		require.NoError(t, err, "zone %s", zones[i])
	}
	assert.Contains(t, objects, "spx3.net.toml")
	assert.Contains(t, objects, "compute.internal.toml")
}

// TestWriterRebuildsCorruptZone covers the recovery half: a zone whose stored
// bytes do not parse must be replaced, not wedge every future write. Both repair
// paths read the zone first, so without this a corrupt object was permanent.
func TestWriterRebuildsCorruptZone(t *testing.T) {
	_, nc, _ := testutil.StartTestJetStream(t)
	shrinkZoneLockWaits(t, time.Second)

	writers, objects := clusterWriters(t, nc, 1)
	objects["spx3.net.toml"] = "version = 1.0\n[domain]\ndomain = \"spx3.ne"

	res, err := writers[0].ApplyBatch(&ChangeBatch{Changes: []Change{{
		Action: ActionUpsert,
		Zone:   "spx3.net",
		Name:   "lb-1.elb.spx3.net",
		Type:   "A",
		Value:  "10.200.1.9",
	}}})
	require.NoError(t, err, "a corrupt zone must be rebuilt, not returned as an error")
	assert.Equal(t, []string{"spx3.net"}, res.Zones)

	cfg := parseZone(t, objects["spx3.net.toml"])
	assert.True(t, hasA(cfg, "10.200.1.9"), "the change that hit the corrupt zone must be applied to the rebuild")
}

// TestWriterCorruptZoneDeleteOnlyIsNoop guards the other branch: a withdrawal
// against a corrupt zone has nothing to materialise, so it must not write a
// stub zone that drops every other record.
func TestWriterCorruptZoneDeleteOnlyIsNoop(t *testing.T) {
	_, nc, _ := testutil.StartTestJetStream(t)
	shrinkZoneLockWaits(t, time.Second)

	writers, objects := clusterWriters(t, nc, 1)
	corrupt := "version = 1.0\n[domain]\ndomain = \"spx3.ne"
	objects["spx3.net.toml"] = corrupt

	res, err := writers[0].ApplyBatch(&ChangeBatch{Changes: []Change{{
		Action: ActionDelete,
		Zone:   "spx3.net",
		Name:   "lb-1.elb.spx3.net",
		Type:   "A",
		Value:  "10.200.1.9",
	}}})
	require.NoError(t, err)
	assert.Empty(t, res.Zones, "a delete-only batch must not materialise a zone")
	assert.Equal(t, corrupt, objects["spx3.net.toml"], "the reconciler's UPSERT set owns the rebuild")
}
