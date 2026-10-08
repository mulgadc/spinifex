package handlers_eks

import (
	"encoding/json"
	eksv1 "github.com/mulgadc/spinifex/contracts/eks/v1"
	"github.com/mulgadc/spinifex/spinifex/domains/eks/addon"
	"testing"
	"time"

	"github.com/mulgadc/spinifex/internal/testkit"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/arn"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// acctKVForTest opens the per-account KV bucket the test service is backed by.
func acctKVForTest(t *testing.T, svc *EKSServiceImpl) jetstream.KeyValue {
	t.Helper()
	js := testutil.NewJetStream(t, svc.deps.NATSConn)
	kv, err := GetOrCreateAccountBucket(t.Context(), js, testAccountID)
	require.NoError(t, err)
	return kv
}

// putAddonRecord seeds a record at its stored key, bypassing the owner.
func putAddonRecord(t *testing.T, kv jetstream.KeyValue, cluster string, rec *addon.Record) error {
	t.Helper()
	data, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	_, err = kv.Put(t.Context(), addon.Key(cluster, rec.AddonName), data)
	return err
}

func TestApplyAddonStatusReport(t *testing.T) {
	svc := setupTestService(t)
	seedTestCluster(t, svc, "c1")
	acctKV := acctKVForTest(t, svc)
	r := &ClusterReconciler{acctKV: acctKV, clusterName: "c1", addonReports: addon.New("", nil)}

	seed := func(t *testing.T) {
		t.Helper()
		now := time.Now().UTC()
		require.NoError(t, putAddonRecord(t, acctKV, "c1", &addon.Record{
			AddonName: "coredns", AddonVersion: "1.11.1", Status: addon.StatusCreating,
			Arn: arn.FormatEKSAddon("us-east-1", testAccountID, "c1", "coredns"), CreatedAt: now, ModifiedAt: now,
		}))
	}
	get := func(t *testing.T) *addon.Record {
		t.Helper()
		rec, err := addon.Get(t.Context(), acctKV, "c1", "coredns")
		require.NoError(t, err)
		return rec
	}

	t.Run("ready flips creating to active and clears health", func(t *testing.T) {
		seed(t)
		r.applyAddonStatusReport(t.Context(), eksv1.AddonStatusReport{Addon: "coredns", Phase: eksv1.AddonPhaseReady, Message: "ignored", TS: time.Now().Unix()})
		rec := get(t)
		assert.Equal(t, addon.StatusActive, rec.Status)
		assert.Empty(t, rec.Health)
	})

	t.Run("failed on active goes degraded and records message", func(t *testing.T) {
		r.applyAddonStatusReport(t.Context(), eksv1.AddonStatusReport{Addon: "coredns", Phase: eksv1.AddonPhaseFailed, Message: "rollout stalled", TS: time.Now().Unix()})
		rec := get(t)
		assert.Equal(t, addon.StatusDegraded, rec.Status)
		assert.Equal(t, "rollout stalled", rec.Health)
	})

	t.Run("applied is a no-op", func(t *testing.T) {
		seed(t)
		before := get(t)
		r.applyAddonStatusReport(t.Context(), eksv1.AddonStatusReport{Addon: "coredns", Phase: eksv1.AddonPhaseApplied, TS: time.Now().Unix()})
		after := get(t)
		assert.Equal(t, addon.StatusCreating, after.Status)
		assert.Equal(t, before.ModifiedAt, after.ModifiedAt, "no-op must not bump ModifiedAt")
	})

	t.Run("unknown addon is a no-op (no panic)", func(t *testing.T) {
		r.applyAddonStatusReport(t.Context(), eksv1.AddonStatusReport{Addon: "not-installed", Phase: eksv1.AddonPhaseReady, TS: time.Now().Unix()})
	})

	t.Run("empty addon is ignored", func(t *testing.T) {
		r.applyAddonStatusReport(t.Context(), eksv1.AddonStatusReport{Phase: eksv1.AddonPhaseReady})
	})
}
