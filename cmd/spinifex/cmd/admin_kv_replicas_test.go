//test:in-package — it tests this command's unexported rendering and repair
// helpers, which exist only to be exercised without a live cluster.

package cmd

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/mulgadc/spinifex/internal/testkit"
	"github.com/mulgadc/spinifex/spinifex/clustersize"
	"github.com/mulgadc/spinifex/spinifex/foundation/state/kvutil"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestWriteReplicaReport_NamesEveryUnderReplicatedBucket(t *testing.T) {
	reports := []kvutil.BucketReport{
		{Bucket: "healthy", Replicas: 3, Want: 3, Online: 3, Leader: "a", Peers: []string{"a", "b", "c"}},
		{Bucket: "short", Replicas: 1, Want: 3, Online: 1, Leader: "a", Peers: []string{"a"}},
		{Bucket: "stuck", Replicas: 3, Want: 3, Online: 1, Peers: []string{"a", "b", "c"}},
	}

	var out bytes.Buffer
	require.NoError(t, writeReplicaReport(&out, reports, false))
	got := out.String()

	assert.Contains(t, got, "BUCKET")
	assert.Contains(t, got, "HELD BY")
	assert.Regexp(t, `healthy\s+3\s+3\s+3\s+ok\s+a\s+a,b,c`, got)
	assert.Regexp(t, `short\s+1\s+3\s+1\s+UNDER-REPLICATED\s+a\s+a`, got)
	// A bucket that cannot serve reads as NO-QUORUM even though its configured
	// count is right, because raising a count is not the response to it.
	assert.Regexp(t, `stuck\s+3\s+3\s+1\s+NO-QUORUM`, got)
	assert.Contains(t, got, "3 buckets, 1 under-replicated, 1 without quorum")
}

// TestReplicaReportExit pins the three exit classes a deployment gate reads. They
// are separate because the responses are: a raise fixes an under-replicated
// bucket, and nothing the command can do fixes one that will not answer.
func TestReplicaReportExit(t *testing.T) {
	healthy := kvutil.BucketReport{Bucket: "a", Replicas: 3, Want: 3, Online: 3}
	short := kvutil.BucketReport{Bucket: "b", Replicas: 1, Want: 3, Online: 1}
	stuck := kvutil.BucketReport{Bucket: "c", Replicas: 3, Want: 3, Online: 1}

	assert.Equal(t, exitHealthy, replicaReportExit(nil))
	assert.Equal(t, exitHealthy, replicaReportExit([]kvutil.BucketReport{healthy}))
	assert.Equal(t, exitUnderReplicated, replicaReportExit([]kvutil.BucketReport{healthy, short}))
	assert.Equal(t, exitUnreachable, replicaReportExit([]kvutil.BucketReport{healthy, stuck}))
	// Both problems at once sends the operator to the node rather than to --repair.
	assert.Equal(t, exitUnreachable, replicaReportExit([]kvutil.BucketReport{short, stuck}))
}

// TestWriteReplicaReport_JSONIsWhatTheE2ESuiteDecodes keeps the machine-readable
// form honest: the e2e suite and anything gating a deployment parse these field
// names, so a rename here breaks them silently.
func TestWriteReplicaReport_JSONIsWhatTheE2ESuiteDecodes(t *testing.T) {
	var out bytes.Buffer
	require.NoError(t, writeReplicaReport(&out, []kvutil.BucketReport{
		{Bucket: "short", Replicas: 1, Want: 3, Cluster: "spinifex", Online: 1, Leader: "a", Peers: []string{"a"}},
	}, true))

	assert.Contains(t, out.String(), `"bucket":"short"`)

	var decoded []kvutil.BucketReport
	require.NoError(t, json.Unmarshal(out.Bytes(), &decoded))
	require.Len(t, decoded, 1)
	assert.Equal(t, "short", decoded[0].Bucket)
	assert.Equal(t, 1, decoded[0].Replicas)
	assert.Equal(t, 3, decoded[0].Want)
	assert.Equal(t, 1, decoded[0].Online)
	assert.Equal(t, "a", decoded[0].Leader)
	assert.True(t, decoded[0].UnderReplicated())
	assert.True(t, decoded[0].HasQuorum())
}

func TestUnderReplicatedCount(t *testing.T) {
	assert.Equal(t, 0, underReplicatedCount(nil))
	assert.Equal(t, 2, underReplicatedCount([]kvutil.BucketReport{
		{Bucket: "a", Replicas: 1, Want: 3},
		{Bucket: "b", Replicas: 3, Want: 3},
		{Bucket: "c", Replicas: 2, Want: 3},
	}))
}

// TestRepairReplicaReports_KeepsGoingPastABucketItCannotPlace is the behaviour
// that makes --repair usable mid-growth: the buckets it can raise are raised,
// and the one it cannot is named rather than aborting the run.
func TestRepairReplicaReports_KeepsGoingPastABucketItCannotPlace(t *testing.T) {
	_, nc, js := testutil.StartTestJetStream(t)
	defer nc.Close()

	for _, b := range []string{"repair-alpha", "repair-beta"} {
		_, err := js.CreateKeyValue(t.Context(), jetstream.KeyValueConfig{Bucket: b, History: 1})
		require.NoError(t, err)
	}

	// One embedded server cannot hold two replicas, so every raise fails — and
	// every bucket must still have been attempted and named.
	reports := []kvutil.BucketReport{
		{Bucket: "repair-alpha", Replicas: 1, Want: 2},
		{Bucket: "repair-beta", Replicas: 1, Want: 2},
	}
	var errOut bytes.Buffer
	repairReplicaReports(t.Context(), js, reports, &errOut)

	assert.Contains(t, errOut.String(), "repair repair-alpha")
	assert.Contains(t, errOut.String(), "repair repair-beta")
	assert.Equal(t, 2, underReplicatedCount(reports), "a failed raise must not be reported as repaired")
}

// TestRepairReplicaReports_RaisesWhatItCan covers the success side against a real
// server, so the in-place report update is proved rather than assumed.
func TestRepairReplicaReports_RaisesWhatItCan(t *testing.T) {
	_, nc, js := testutil.StartTestJetStream(t)
	defer nc.Close()
	clustersize.DeclareForTest(t, 1)

	_, err := js.CreateKeyValue(t.Context(), jetstream.KeyValueConfig{Bucket: "repair-ok", History: 1})
	require.NoError(t, err)

	// Want 1 against a bucket already at 1: the raise is a no-op that must still
	// leave the report reading as healthy rather than as repaired-and-wrong.
	reports := []kvutil.BucketReport{{Bucket: "repair-ok", Replicas: 1, Want: 1}}
	var errOut bytes.Buffer
	repairReplicaReports(t.Context(), js, reports, &errOut)

	assert.Empty(t, strings.TrimSpace(errOut.String()))
	assert.Equal(t, 0, underReplicatedCount(reports))
}
