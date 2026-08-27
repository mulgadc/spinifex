package dns

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	nsconfig "github.com/mulgadc/northstar/pkg/config"
	"github.com/mulgadc/spinifex/spinifex/kvstore"
	"github.com/mulgadc/spinifex/spinifex/reconciler"
	"github.com/mulgadc/spinifex/spinifex/testutil"
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

func TestComputeConvergePrunesRDSOnlyWhenEnumerated(t *testing.T) {
	desired := []Change{upsert("orders-db.111122223333.ap-southeast-2.rds.spx3.net", "10.0.5.20")}
	existing := map[string][]zoneRecord{testBase: {
		existingA("orders-db.111122223333.ap-southeast-2.rds.", "10.0.5.20"),
		existingA("dropped-db.111122223333.ap-southeast-2.rds.", "10.0.5.21"),
		existingA("app-web-abc.ap-southeast-2.elb.", "1.1.1.1"),
		existingA("ec2-4-4-4-4.ap-southeast-2.compute.", "4.4.4.4"),
	}}

	// Without RDS authority the deleted instance's record survives, and so does
	// every other class this cycle could not enumerate.
	batch, err := computeConverge(desired, existing, prunableFor(PruneScope{}))
	require.NoError(t, err)
	assert.Empty(t, deletesOf(batch), "RDS pruning is suppressed when the account buckets were not fully read")

	batch, err = computeConverge(desired, existing, prunableFor(PruneScope{RDS: true}))
	require.NoError(t, err)
	deletes := deletesOf(batch)

	require.Len(t, deletes, 1, "only the DB instance absent from the desired set is pruned")
	assert.Equal(t, "dropped-db.111122223333.ap-southeast-2.rds.spx3.net", deletes[0].Name)
	for _, d := range deletes {
		assert.NotContains(t, d.Name, ".elb.", "RDS authority does not grant ELB pruning")
		assert.NotContains(t, d.Name, ".compute.", "EC2 records are never pruned")
	}
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

func TestSplitReconcileChanges(t *testing.T) {
	tests := []struct {
		name    string
		changes func() []Change
	}{
		{
			name: "record element limit",
			changes: func() []Change {
				changes := make([]Change, 501)
				for i := range changes {
					changes[i] = upsert(fmt.Sprintf("record-%03d.spx3.net", len(changes)-i), "192.0.2.1")
				}
				return changes
			},
		},
		{
			name: "value character limit",
			changes: func() []Change {
				return []Change{
					upsert("first.spx3.net", strings.Repeat("a", 9_000)),
					upsert("second.spx3.net", strings.Repeat("b", 9_000)),
				}
			},
		},
		{
			name: "serialized payload limit",
			changes: func() []Change {
				changes := make([]Change, 100)
				for i := range changes {
					changes[i] = upsert(fmt.Sprintf("%s-%03d.spx3.net", strings.Repeat("n", 10_000), len(changes)-i), "192.0.2.1")
				}
				return changes
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			changes := tt.changes()
			batches, err := splitReconcileChanges(changes, maxReconcileNATSPayloadBytes)
			require.NoError(t, err)
			require.Greater(t, len(batches), 1)

			var flattened []Change
			for _, batch := range batches {
				flattened = append(flattened, batch...)
				payload, err := json.Marshal(ChangeBatch{Changes: batch})
				require.NoError(t, err)
				assert.LessOrEqual(t, len(payload), maxReconcileNATSPayloadBytes)

				records, valueChars := 0, 0
				for _, change := range batch {
					multiplier := 1
					if change.Action == ActionUpsert {
						multiplier = 2
					}
					records += multiplier
					valueChars += multiplier * len([]rune(change.Value))
				}
				assert.LessOrEqual(t, records, MaxRecordsPerChangeRequest)
				assert.LessOrEqual(t, valueChars, MaxValueCharsPerChangeRequest)
			}
			assert.Equal(t, changes, flattened, "batching must preserve change order")
		})
	}
}

func TestSplitReconcileChangesExactBoundaries(t *testing.T) {
	t.Run("record elements", func(t *testing.T) {
		changes := make([]Change, MaxRecordsPerChangeRequest/2)
		for i := range changes {
			changes[i] = upsert(fmt.Sprintf("record-%d.spx3.net", i), "192.0.2.1")
		}
		batches, err := splitReconcileChanges(changes, maxReconcileNATSPayloadBytes)
		require.NoError(t, err)
		require.Len(t, batches, 1)

		overLimit := make([]Change, len(changes)+1)
		copy(overLimit, changes)
		overLimit[len(changes)] = upsert("one-over.spx3.net", "192.0.2.2")
		batches, err = splitReconcileChanges(overLimit, maxReconcileNATSPayloadBytes)
		require.NoError(t, err)
		assert.Len(t, batches, 2)
	})

	t.Run("value characters", func(t *testing.T) {
		change := upsert("exact.spx3.net", strings.Repeat("界", MaxValueCharsPerChangeRequest/2))
		batches, err := splitReconcileChanges([]Change{change}, maxReconcileNATSPayloadBytes)
		require.NoError(t, err)
		require.Len(t, batches, 1)
		assert.Equal(t, change, batches[0][0])
	})

	t.Run("serialized payload", func(t *testing.T) {
		change := upsert("", "192.0.2.1")
		encoded, err := json.Marshal(change)
		require.NoError(t, err)
		nameBytes := maxReconcileNATSPayloadBytes - changeBatchJSONOverhead - len(encoded)
		require.Positive(t, nameBytes)
		change.Name = strings.Repeat("n", nameBytes)

		payload, err := json.Marshal(ChangeBatch{Changes: []Change{change}})
		require.NoError(t, err)
		require.Len(t, payload, maxReconcileNATSPayloadBytes)
		batches, err := splitReconcileChanges([]Change{change}, maxReconcileNATSPayloadBytes)
		require.NoError(t, err)
		require.Len(t, batches, 1)

		change.Name += "n"
		_, err = splitReconcileChanges([]Change{change}, maxReconcileNATSPayloadBytes)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "payload maximum")
	})
}

func TestSplitReconcileChangesTracksRequestLimitsPerZone(t *testing.T) {
	changes := make([]Change, MaxRecordsPerChangeRequest)
	for i := range changes {
		zone := "spx3.net"
		if i%2 == 1 {
			zone = "compute.internal"
		}
		changes[i] = Change{Action: ActionUpsert, Zone: zone, Name: fmt.Sprintf("record-%d.%s", i, zone), Type: "A", Value: "192.0.2.1"}
	}

	batches, err := splitReconcileChanges(changes, maxReconcileNATSPayloadBytes)
	require.NoError(t, err)
	require.Len(t, batches, 1, "each zone independently stays at 1,000 record elements")
}

func TestSplitAndPublishReconcileChangesHandlesEmptyAndSingleton(t *testing.T) {
	batches, err := splitReconcileChanges(nil, maxReconcileNATSPayloadBytes)
	require.NoError(t, err)
	assert.Empty(t, batches)

	calls := 0
	err = publishReconcileBatches(nil, maxReconcileNATSPayloadBytes, func([]Change) error {
		calls++
		return nil
	})
	require.NoError(t, err)
	assert.Zero(t, calls)

	change := upsert("one.spx3.net", "192.0.2.1")
	err = publishReconcileBatches([]Change{change}, maxReconcileNATSPayloadBytes, func(batch []Change) error {
		calls++
		assert.Equal(t, []Change{change}, batch)
		return nil
	})
	require.NoError(t, err)
	assert.Equal(t, 1, calls)
}

func TestPublishReconcileBatchRejectsMissingAcknowledgement(t *testing.T) {
	err := publishReconcileBatch(nil, "000000000000", []Change{upsert("one.spx3.net", "192.0.2.1")})
	require.Error(t, err)
	assert.ErrorContains(t, err, "writer acknowledged 0 of 1 changes")
}

func TestSplitReconcileChangesRejectsOversizedChange(t *testing.T) {
	changes := []Change{upsert("oversized.spx3.net", strings.Repeat("x", MaxValueCharsPerChangeRequest/2+1))}

	_, err := splitReconcileChanges(changes, maxReconcileNATSPayloadBytes)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "value characters")
}

func TestPublishReconcileBatchesWaitsForEachAcknowledgement(t *testing.T) {
	changes := make([]Change, 501)
	for i := range changes {
		changes[i] = upsert(fmt.Sprintf("record-%d.spx3.net", i), "192.0.2.1")
	}
	firstStarted := make(chan struct{})
	secondStarted := make(chan struct{})
	releaseFirst := make(chan struct{})
	done := make(chan error, 1)
	calls := 0

	go func() {
		done <- publishReconcileBatches(changes, maxReconcileNATSPayloadBytes, func([]Change) error {
			calls++
			switch calls {
			case 1:
				close(firstStarted)
				<-releaseFirst
			case 2:
				close(secondStarted)
			}
			return nil
		})
	}()

	<-firstStarted
	select {
	case <-secondStarted:
		t.Fatal("second batch started before the first was acknowledged")
	case <-time.After(25 * time.Millisecond):
	}
	close(releaseFirst)
	require.NoError(t, <-done)
	assert.Equal(t, 2, calls)
	select {
	case <-secondStarted:
	default:
		t.Fatal("second batch was not published after the first acknowledgement")
	}
}

func TestPublishReconcileBatchesStopsAfterFailedAcknowledgement(t *testing.T) {
	changes := make([]Change, 1_001)
	for i := range changes {
		changes[i] = upsert("record.spx3.net", "192.0.2.1")
	}
	publishErr := errors.New("writer unavailable")
	calls := 0

	err := publishReconcileBatches(changes, maxReconcileNATSPayloadBytes, func([]Change) error {
		calls++
		if calls == 2 {
			return publishErr
		}
		return nil
	})

	require.ErrorIs(t, err, publishErr)
	assert.ErrorContains(t, err, "publish batch 2 of 3")
	assert.Equal(t, 2, calls, "later batches must wait for and stop after a failed acknowledgement")
}

func TestReconcilerDisabledIsNoop(t *testing.T) {
	r := &Reconciler{} // enabled=false
	assert.False(t, r.Enabled())
	r.reconcileOnce(t.Context()) // must not panic with a nil desired/S3
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

// TestReconcilerCorruptZoneRebuildsWithoutPruning covers the recovery half of the
// zone-write race. A corrupt zone must not wedge the backstop: it reports the zone
// as absent so the desired set is still published and the writer rebuilds, and
// because nothing was read, no record can look stale and be pruned away.
func TestReconcilerCorruptZoneRebuildsWithoutPruning(t *testing.T) {
	endpoint, objects := fakeS3(t, "northstar")
	objects[testBase+".toml"] = "version = 1.0\n[domain]\ndomain = \"spx3.ne"

	r := &Reconciler{
		enabled:    true,
		baseDomain: testBase,
		s3cfg: &nsconfig.S3Config{
			Endpoint: endpoint, Bucket: "northstar", Region: "us-east-1",
			AccessKey: "SYSTEM", SecretKey: "SYSTEMSECRET",
		},
		desired: func() DesiredSet {
			return DesiredSet{
				Changes:  []Change{upsert("lb-1.elb."+testBase, "10.200.1.9")},
				Prunable: PruneScope{ELB: true, EKS: true, RDS: true},
			}
		},
	}

	recs, ok, err := r.readZone(testBase)
	require.NoError(t, err, "a corrupt zone must not abort the cycle")
	assert.False(t, ok, "a corrupt zone reports as absent so nothing is pruned against it")
	assert.Empty(t, recs)

	batch, err := r.computeBatch()
	require.NoError(t, err)
	assert.Equal(t, []Change{upsert("lb-1.elb."+testBase, "10.200.1.9")}, batch,
		"the full desired set must still be published so the writer rebuilds the zone")
	assert.Empty(t, deletesOf(batch), "a corrupt zone must never produce deletes")
}

// TestReconcilerBackendErrorStillAborts guards the other side: an unreachable
// backend must not be mistaken for corrupt bytes and trigger a rebuild.
func TestReconcilerBackendErrorStillAborts(t *testing.T) {
	r := &Reconciler{enabled: true, baseDomain: testBase, s3cfg: &nsconfig.S3Config{}}
	_, _, err := r.readZone(testBase)
	require.Error(t, err, "a backend failure must propagate, not look like a rebuildable zone")
}

// TestReconciler_RunReturnsWhenDisabled pins the guard that keeps the daemon
// able to start the loop unconditionally. Without it the disabled reconciler
// would enter the watch loop and block until shutdown instead of returning.
func TestReconciler_RunReturnsWhenDisabled(t *testing.T) {
	r := NewReconciler(nil, nil, nil)
	require.False(t, r.Enabled())

	done := make(chan struct{})
	go func() {
		defer close(done)
		r.Run(t.Context())
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return for a disabled reconciler")
	}
}

// TestReconciler_RunPerformsStartupPassThenStopsOnCancel covers the enabled
// path of Run: the startup pass must fire before any watch or resync activity,
// and cancelling ctx must still let the loop return.
func TestReconciler_RunPerformsStartupPassThenStopsOnCancel(t *testing.T) {
	_, nc, _ := testutil.StartTestJetStream(t)
	endpoint, _ := fakeS3(t, "northstar")

	var passes atomic.Int64
	r := &Reconciler{
		enabled:    true,
		baseDomain: testBase,
		s3cfg: &nsconfig.S3Config{
			Endpoint: endpoint, Bucket: "northstar", Region: "us-east-1",
			AccessKey: "SYSTEM", SecretKey: "SYSTEMSECRET",
		},
		nc:     nc,
		holder: "test-node",
		desired: func() DesiredSet {
			passes.Add(1)
			return DesiredSet{}
		},
	}

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		defer close(done)
		r.Run(ctx)
	}()

	require.Eventually(t, func() bool { return passes.Load() >= 1 }, 5*time.Second, 10*time.Millisecond,
		"the startup pass must run immediately, before any watch or resync activity")

	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after ctx was cancelled")
	}
}

// TestNewReconciler_RetainsItsWatchSources covers the wiring the daemon relies
// on: sources supplied at construction have to reach the loop, and supplying
// none is legal because that is the interval-only behaviour.
func TestNewReconciler_RetainsItsWatchSources(t *testing.T) {
	bucket := kvstore.NewBucket(nil, kvstore.Config{Name: "b"})

	assert.Empty(t, NewReconciler(nil, nil, nil).sources)
	assert.Len(t, NewReconciler(nil, nil, nil,
		reconciler.Fixed(bucket, "node.*"),
		reconciler.Fixed(bucket, "lb.*"),
	).sources, 2)
}
