package handlers_eks

import (
	"context"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/s3"
	"github.com/mulgadc/spinifex/spinifex/objectstore"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseEtcdSnapshotKey(t *testing.T) {
	cases := []struct {
		name     string
		basename string
		wantOK   bool
		wantTier string
		wantTS   string
	}{
		{"frequent tier", "etcd-frequent-20260709T010000Z.snap", true, "frequent", "20260709T010000Z"},
		{"daily tier", "etcd-daily-20260601T000000Z.snap", true, "daily", "20260601T000000Z"},
		{"missing prefix", "frequent-20260709T010000Z.snap", false, "", ""},
		{"missing suffix", "etcd-frequent-20260709T010000Z.tar", false, "", ""},
		{"no tier separator", "etcd-20260709T010000Z.snap", false, "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			parsed, ok := parseEtcdSnapshotKey(tc.basename)
			require.Equal(t, tc.wantOK, ok)
			if tc.wantOK {
				assert.Equal(t, tc.wantTier, parsed.tier)
				assert.Equal(t, tc.wantTS, parsed.timestamp)
			}
		})
	}
}

func seedSnapshot(t *testing.T, store objectstore.ObjectStore, accountID, cluster, basename string) {
	t.Helper()
	key := accountID + "/" + cluster + "/" + basename
	_, err := store.PutObject(context.Background(), &s3.PutObjectInput{
		Bucket: aws.String(eksBackupsBucket),
		Key:    aws.String(key),
		Body:   strings.NewReader("fake-snapshot-data"),
	})
	require.NoError(t, err)
}

func TestResolveLatestSnapshot_PrefersFrequentTier(t *testing.T) {
	store := objectstore.NewMemoryObjectStore()
	seedSnapshot(t, store, testAccountID, "alpha", "etcd-daily-20260709T230000Z.snap")
	seedSnapshot(t, store, testAccountID, "alpha", "etcd-frequent-20260709T010000Z.snap")
	seedSnapshot(t, store, testAccountID, "alpha", "etcd-frequent-20260709T020000Z.snap")
	// A different cluster's snapshot must not be considered.
	seedSnapshot(t, store, testAccountID, "beta", "etcd-frequent-20260710T230000Z.snap")

	got, err := resolveLatestSnapshot(context.Background(), store, testAccountID, "alpha")
	require.NoError(t, err)
	assert.Equal(t, "etcd-frequent-20260709T020000Z.snap", got, "newest frequent-tier snapshot wins even though a newer daily exists")
}

func TestResolveLatestSnapshot_FallsBackToAnyTierWhenNoFrequent(t *testing.T) {
	store := objectstore.NewMemoryObjectStore()
	seedSnapshot(t, store, testAccountID, "alpha", "etcd-daily-20260601T000000Z.snap")
	seedSnapshot(t, store, testAccountID, "alpha", "etcd-daily-20260602T000000Z.snap")

	got, err := resolveLatestSnapshot(context.Background(), store, testAccountID, "alpha")
	require.NoError(t, err)
	assert.Equal(t, "etcd-daily-20260602T000000Z.snap", got)
}

func TestResolveLatestSnapshot_NoStoreConfigured(t *testing.T) {
	_, err := resolveLatestSnapshot(context.Background(), nil, testAccountID, "alpha")
	require.Error(t, err)
}

func TestResolveLatestSnapshot_NoSnapshotsFound(t *testing.T) {
	store := objectstore.NewMemoryObjectStore()
	_, err := resolveLatestSnapshot(context.Background(), store, testAccountID, "alpha")
	require.Error(t, err)
}

// restoreSnapshotFixtureMeta builds a single-CP ClusterMeta with everything
// RestoreSnapshot needs: a persisted launch template and NLB target-group ARNs.
func restoreSnapshotFixtureMeta(name string) *ClusterMeta {
	tmpl := validK3sInput()
	tmpl.AccessKey = ""
	tmpl.SecretKey = ""
	tmpl.PredastoreAccessKey = ""
	tmpl.PredastoreSecretKey = ""

	meta := sampleClusterMeta(name)
	meta.NLBTargetGroupArn = "arn:aws:elasticloadbalancing:us-east-1:111122223333:targetgroup/" + ClusterTargetGroupName(name) + "/tg-001"
	meta.KonnTargetGroupArn = "arn:aws:elasticloadbalancing:us-east-1:111122223333:targetgroup/" + ClusterTargetGroupName(name) + "-konn/tg-002"
	meta.ControlPlaneTemplate = &tmpl
	meta.ControlPlaneNodes = []ControlPlaneNode{{
		NodeID:     "node-old",
		InstanceID: "i-old111",
		ENIID:      "eni-old111",
		ENIIP:      "10.0.1.9",
		MgmtIP:     "10.255.0.5",
	}}
	meta.ControlPlaneInstanceID = "i-old111"
	meta.ControlPlaneENIID = "eni-old111"
	meta.ControlPlaneENIIP = "10.0.1.9"
	return meta
}

func TestRestoreSnapshot_HappyPathLaunchesDirectsAndRepoints(t *testing.T) {
	f := newEKSServiceFixture(t)
	f.svc.deps.Scheduler = &fakeHostScheduler{hosts: []string{"node-new"}}
	store := objectstore.NewMemoryObjectStore()
	f.svc.deps.SnapshotStore = store
	seedSnapshot(t, store, testAccountID, "alpha", "etcd-frequent-20260709T010000Z.snap")

	meta := restoreSnapshotFixtureMeta("alpha")
	require.NoError(t, PutClusterMeta(f.kv, meta))

	out, err := f.svc.RestoreSnapshot(context.Background(), &RestoreSnapshotInput{ClusterName: "alpha"}, testAccountID)
	require.NoError(t, err)
	assert.Equal(t, "etcd-frequent-20260709T010000Z.snap", out.Snapshot, "empty --snapshot resolves the latest one")
	assert.NotEmpty(t, out.NewInstanceID)

	// The recovery directive must be wired for the new instance.
	dirOut, err := f.svc.GetRecoveryDirective(context.Background(),
		&GetRecoveryDirectiveInput{ClusterName: "alpha", InstanceID: out.NewInstanceID}, testAccountID)
	require.NoError(t, err)
	assert.Equal(t, RecoveryActionClusterReset, dirOut.Directive.Action)
	assert.Equal(t, "etcd-frequent-20260709T010000Z.snap", dirOut.Directive.Snapshot)

	// NLB re-pointed: old CP deregistered, new CP registered, on both TGs.
	require.Len(t, f.nlb.deregisterCalls, 2, "apiserver + konnectivity TGs")
	require.Len(t, f.nlb.registerCalls, 2)
	for _, dc := range f.nlb.deregisterCalls {
		require.Len(t, dc.Targets, 1)
		assert.Equal(t, "10.0.1.9", aws.StringValue(dc.Targets[0].Id), "deregisters the OLD CP's ENI IP")
	}
	for _, rc := range f.nlb.registerCalls {
		require.Len(t, rc.Targets, 1)
		assert.NotEqual(t, "10.0.1.9", aws.StringValue(rc.Targets[0].Id), "registers the NEW CP's ENI IP, not the old one")
	}

	// Cluster meta reflects the replacement.
	got, err := GetClusterMeta(f.kv, "alpha")
	require.NoError(t, err)
	assert.Equal(t, out.NewInstanceID, got.ControlPlaneInstanceID)
	require.Len(t, got.ControlPlaneNodes, 1)
	assert.Equal(t, out.NewInstanceID, got.ControlPlaneNodes[0].InstanceID)

	// The old CP is torn down best-effort.
	assert.Contains(t, f.inst.terminateCalls, "i-old111")
}

func TestRestoreSnapshot_ExplicitSnapshotUsedVerbatim(t *testing.T) {
	f := newEKSServiceFixture(t)
	f.svc.deps.Scheduler = &fakeHostScheduler{hosts: []string{"node-new"}}
	meta := restoreSnapshotFixtureMeta("alpha")
	require.NoError(t, PutClusterMeta(f.kv, meta))

	out, err := f.svc.RestoreSnapshot(context.Background(),
		&RestoreSnapshotInput{ClusterName: "alpha", Snapshot: "etcd-daily-20260601T000000Z.snap"}, testAccountID)
	require.NoError(t, err)
	assert.Equal(t, "etcd-daily-20260601T000000Z.snap", out.Snapshot, "explicit snapshot is used verbatim, no store lookup")
}

func TestRestoreSnapshot_HARejected(t *testing.T) {
	f := newEKSServiceFixture(t)
	meta := restoreSnapshotFixtureMeta("alpha")
	meta.ControlPlaneSpreadGroup = "eks-cp-111122223333-alpha"
	meta.ControlPlaneNodes = append(meta.ControlPlaneNodes, ControlPlaneNode{NodeID: "node-2", InstanceID: "i-cp2"})
	require.NoError(t, PutClusterMeta(f.kv, meta))

	_, err := f.svc.RestoreSnapshot(context.Background(),
		&RestoreSnapshotInput{ClusterName: "alpha", Snapshot: "etcd-daily-20260601T000000Z.snap"}, testAccountID)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "HA spread")
	assert.Empty(t, f.inst.launchCalls, "must not launch anything for an HA cluster")
}

func TestRestoreSnapshot_NoTemplateRejected(t *testing.T) {
	f := newEKSServiceFixture(t)
	meta := restoreSnapshotFixtureMeta("alpha")
	meta.ControlPlaneTemplate = nil
	require.NoError(t, PutClusterMeta(f.kv, meta))

	_, err := f.svc.RestoreSnapshot(context.Background(),
		&RestoreSnapshotInput{ClusterName: "alpha", Snapshot: "etcd-daily-20260601T000000Z.snap"}, testAccountID)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "template")
}

func TestRestoreSnapshot_ClusterNotFound(t *testing.T) {
	f := newEKSServiceFixture(t)
	_, err := f.svc.RestoreSnapshot(context.Background(),
		&RestoreSnapshotInput{ClusterName: "ghost", Snapshot: "etcd-daily-20260601T000000Z.snap"}, testAccountID)
	require.Error(t, err)
}
