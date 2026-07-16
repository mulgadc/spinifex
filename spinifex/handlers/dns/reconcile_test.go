package dns

import (
	"testing"

	nsconfig "github.com/mulgadc/northstar/pkg/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const testBase = "spx3.net"

// prunableFor mirrors Reconciler.prunable without a live config, so the pure
// converge logic can be exercised for a given tenant-enumeration scope.
func prunableFor(scope PruneScope) func(zone, label string) bool {
	r := &Reconciler{baseDomain: testBase}
	return r.prunable(scope)
}

func upsert(name, value string) Change {
	return Change{Action: ActionUpsert, Zone: testBase, Name: name, Type: "A", Value: value}
}

func existingA(label, value string) zoneRecord {
	return zoneRecord{label: label, rtype: nsconfig.TypeA, value: value}
}

func TestComputeConvergeUpsertsPassThroughAndPruneStale(t *testing.T) {
	desired := []Change{
		upsert("app-web-abc.ap-southeast-2.elb.spx3.net", "1.1.1.1"),
		upsert("prod.ap-southeast-2.eks.spx3.net", "2.2.2.2"),
		upsert("ec2-3-3-3-3.ap-southeast-2.compute.spx3.net", "3.3.3.3"),
	}
	existing := map[string][]zoneRecord{testBase: {
		// A stale load balancer no longer in the desired set → must be pruned.
		existingA("app-old-xyz.ap-southeast-2.elb.", "9.9.9.9"),
		// A live EKS record still desired → kept (dedup by RRset, not re-deleted).
		existingA("prod.ap-southeast-2.eks.", "2.2.2.2"),
		// An EC2 record absent from this node's desired view → must NOT be pruned.
		existingA("ec2-4-4-4-4.ap-southeast-2.compute.", "4.4.4.4"),
		// Structural apex NS → never pruned.
		{label: "", rtype: nsconfig.TypeNS, value: "ns1.spx3.net."},
	}}

	batch, err := computeConverge(desired, existing, prunableFor(PruneScope{ELB: true, EKS: true}))
	require.NoError(t, err)

	// All three desired upserts pass through unchanged.
	assert.Equal(t, desired, batch[:3])

	deletes := deletesOf(batch)
	require.Len(t, deletes, 1, "only the stale load balancer is pruned")
	assert.Equal(t, "app-old-xyz.ap-southeast-2.elb.spx3.net", deletes[0].Name)
	assert.Equal(t, "9.9.9.9", deletes[0].Value)

	// EC2 and structural records survive.
	for _, d := range deletes {
		assert.NotContains(t, d.Name, ".compute.", "EC2 records are never pruned")
		assert.NotEmpty(t, d.Name, "structural apex records are never pruned")
	}
}

func TestComputeConvergeSuppressesPruneWhenClassNotEnumerated(t *testing.T) {
	// A cycle where the EKS enumeration failed (Prunable.EKS=false) must not
	// delete any EKS record even though none appear in the desired set — the
	// partial view could belong to another tenant whose buckets we could not read.
	desired := []Change{upsert("app-web-abc.ap-southeast-2.elb.spx3.net", "1.1.1.1")}
	existing := map[string][]zoneRecord{testBase: {
		existingA("app-stale-xyz.ap-southeast-2.elb.", "9.9.9.9"),
		existingA("prod.ap-southeast-2.eks.", "2.2.2.2"),
	}}

	batch, err := computeConverge(desired, existing, prunableFor(PruneScope{ELB: true, EKS: false}))
	require.NoError(t, err)
	deletes := deletesOf(batch)

	require.Len(t, deletes, 1, "ELB is authoritative so its stale record prunes")
	assert.Contains(t, deletes[0].Name, ".elb.")
	for _, d := range deletes {
		assert.NotContains(t, d.Name, ".eks.", "EKS pruning suppressed when not enumerated")
	}
}

func TestComputeConvergeNoPruneWhenNoAuthority(t *testing.T) {
	// Total enumeration failure (both flags false) must never delete anything,
	// even against a full zone — protects every tenant from a transient outage.
	existing := map[string][]zoneRecord{testBase: {
		existingA("app-a.ap-southeast-2.elb.", "1.1.1.1"),
		existingA("prod.ap-southeast-2.eks.", "2.2.2.2"),
	}}
	batch, err := computeConverge(nil, existing, prunableFor(PruneScope{}))
	require.NoError(t, err)
	assert.Empty(t, deletesOf(batch), "no authority means no deletions")
}

func TestComputeConvergeRejectsUnsupportedRecordType(t *testing.T) {
	desired := []Change{{
		Action: ActionUpsert,
		Zone:   testBase,
		Name:   "host.spx3.net",
		Type:   "AAAA",
		Value:  "2001:db8::1",
	}}

	_, err := computeConverge(desired, nil, prunableFor(PruneScope{}))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unsupported DNS record type")
}

func TestReconcilerDisabledIsNoop(t *testing.T) {
	r := &Reconciler{} // enabled=false
	assert.False(t, r.Enabled())
	r.reconcileOnce() // must not panic with a nil desired/S3
}

func deletesOf(batch []Change) []Change {
	var out []Change
	for _, c := range batch {
		if c.Action == ActionDelete {
			out = append(out, c)
		}
	}
	return out
}
