package handlers_eks

import (
	"testing"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// acctKVForTest opens the per-account KV bucket the test service is backed by.
func acctKVForTest(t *testing.T, svc *EKSServiceImpl) nats.KeyValue {
	t.Helper()
	js, err := svc.deps.NATSConn.JetStream()
	require.NoError(t, err)
	kv, err := GetOrCreateAccountBucket(js, testAccountID)
	require.NoError(t, err)
	return kv
}

func TestNextAddonStatus(t *testing.T) {
	cases := []struct {
		name        string
		cur         AddonStatus
		phase       AddonDeliveryPhase
		wantStatus  AddonStatus
		wantChanged bool
	}{
		{"ready from creating -> active", AddonStatusCreating, AddonPhaseReady, AddonStatusActive, true},
		{"ready from updating -> active", AddonStatusUpdating, AddonPhaseReady, AddonStatusActive, true},
		{"ready when already active -> no-op", AddonStatusActive, AddonPhaseReady, AddonStatusActive, false},
		{"applied is informational -> no-op", AddonStatusCreating, AddonPhaseApplied, AddonStatusCreating, false},
		{"failed from creating -> create_failed", AddonStatusCreating, AddonPhaseFailed, AddonStatusCreateFailed, true},
		{"failed from updating -> create_failed", AddonStatusUpdating, AddonPhaseFailed, AddonStatusCreateFailed, true},
		{"failed when active -> degraded", AddonStatusActive, AddonPhaseFailed, AddonStatusDegraded, true},
		{"failed when degraded -> sticky", AddonStatusDegraded, AddonPhaseFailed, AddonStatusDegraded, false},
		{"failed when create_failed -> sticky", AddonStatusCreateFailed, AddonPhaseFailed, AddonStatusCreateFailed, false},
		{"unknown phase -> no-op", AddonStatusCreating, AddonDeliveryPhase("bogus"), AddonStatusCreating, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, changed := nextAddonStatus(tc.cur, tc.phase)
			assert.Equal(t, tc.wantStatus, got)
			assert.Equal(t, tc.wantChanged, changed)
		})
	}
}

func TestApplyAddonStatusReport(t *testing.T) {
	svc := setupTestService(t)
	seedTestCluster(t, svc, "c1")
	acctKV := acctKVForTest(t, svc)
	r := &ClusterReconciler{acctKV: acctKV, clusterName: "c1"}

	seed := func(t *testing.T) {
		t.Helper()
		now := time.Now().UTC()
		require.NoError(t, PutAddonRecord(acctKV, "c1", &AddonRecord{
			AddonName: "coredns", AddonVersion: "1.11.1", Status: AddonStatusCreating,
			Arn: AddonARN("us-east-1", testAccountID, "c1", "coredns"), CreatedAt: now, ModifiedAt: now,
		}))
	}
	get := func(t *testing.T) *AddonRecord {
		t.Helper()
		rec, err := GetAddonRecord(acctKV, "c1", "coredns")
		require.NoError(t, err)
		return rec
	}

	t.Run("ready flips creating to active and clears health", func(t *testing.T) {
		seed(t)
		r.applyAddonStatusReport(AddonStatusReport{Addon: "coredns", Phase: AddonPhaseReady, Message: "ignored", TS: time.Now().Unix()})
		rec := get(t)
		assert.Equal(t, AddonStatusActive, rec.Status)
		assert.Empty(t, rec.Health)
	})

	t.Run("failed on active goes degraded and records message", func(t *testing.T) {
		r.applyAddonStatusReport(AddonStatusReport{Addon: "coredns", Phase: AddonPhaseFailed, Message: "rollout stalled", TS: time.Now().Unix()})
		rec := get(t)
		assert.Equal(t, AddonStatusDegraded, rec.Status)
		assert.Equal(t, "rollout stalled", rec.Health)
	})

	t.Run("applied is a no-op", func(t *testing.T) {
		seed(t)
		before := get(t)
		r.applyAddonStatusReport(AddonStatusReport{Addon: "coredns", Phase: AddonPhaseApplied, TS: time.Now().Unix()})
		after := get(t)
		assert.Equal(t, AddonStatusCreating, after.Status)
		assert.Equal(t, before.ModifiedAt, after.ModifiedAt, "no-op must not bump ModifiedAt")
	})

	t.Run("unknown addon is a no-op (no panic)", func(t *testing.T) {
		r.applyAddonStatusReport(AddonStatusReport{Addon: "not-installed", Phase: AddonPhaseReady, TS: time.Now().Unix()})
	})

	t.Run("empty addon is ignored", func(t *testing.T) {
		r.applyAddonStatusReport(AddonStatusReport{Phase: AddonPhaseReady})
	})
}
