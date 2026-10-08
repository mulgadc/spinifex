package handlers_eks

import (
	"context"
	"encoding/json"
	"errors"
	eksv1 "github.com/mulgadc/spinifex/contracts/eks/v1"
	"github.com/mulgadc/spinifex/spinifex/domains/eks/addon"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/eks"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	addonLifecycleRegion  = "ap-southeast-2"
	addonLifecycleRoleARN = "arn:aws:iam::111122223333:role/alb"
	albRecordKey          = "clusters/c1/addons/aws-load-balancer-controller"
	albManifestKey        = "clusters/c1/addons/aws-load-balancer-controller/manifest"
	albARN                = "arn:aws:eks:ap-southeast-2:111122223333:addon/c1/aws-load-balancer-controller"
)

// setupStagingAddonService wires the default staging installer (no fake), so
// the manifest sub-key is written exactly as production writes it.
func setupStagingAddonService(t *testing.T) (*EKSServiceImpl, jetstream.KeyValue) {
	t.Helper()
	svc := setupTestService(t)
	svc.deps.Region = addonLifecycleRegion
	seedTestCluster(t, svc, "c1")
	return svc, acctKVForTest(t, svc)
}

func rawJSONObject(t *testing.T, kv jetstream.KeyValue, key string) (map[string]json.RawMessage, []byte) {
	t.Helper()
	entry, err := kv.Get(t.Context(), key)
	require.NoError(t, err, "key %s", key)
	var top map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(entry.Value(), &top))
	return top, entry.Value()
}

// Reads the record and manifest back by literal key. Conflict, pod-identity and
// client-token inputs are supplied to show none of them is persisted anywhere.
func TestAddonPersistedLayout_RecordAndManifestKeysAndFieldNames(t *testing.T) {
	t.Parallel()
	svc, kv := setupStagingAddonService(t)

	out, err := svc.CreateAddon(context.Background(), &eks.CreateAddonInput{
		ClusterName:           aws.String("c1"),
		AddonName:             aws.String(albController),
		ServiceAccountRoleArn: aws.String(addonLifecycleRoleARN),
		ConfigurationValues:   aws.String(`{"replicaCount":2}`),
		Tags:                  aws.StringMap(map[string]string{"team": "platform"}),
		ResolveConflicts:      aws.String(eks.ResolveConflictsOverwrite),
		ClientRequestToken:    aws.String("tok-1"),
		PodIdentityAssociations: []*eks.AddonPodIdentityAssociations{{
			RoleArn: aws.String(addonLifecycleRoleARN), ServiceAccount: aws.String("alb"),
		}},
	}, testAccountID)
	require.NoError(t, err)
	assert.Equal(t, albARN, aws.StringValue(out.Addon.AddonArn))

	rec, _ := rawJSONObject(t, kv, albRecordKey)
	assert.ElementsMatch(t, []string{
		"addonName", "addonVersion", "status", "serviceAccountRoleArn", "configurationValues",
		"arn", "tags", "createdAt", "modifiedAt",
	}, jsonObjectKeys(rec))
	assert.JSONEq(t, `"CREATING"`, string(rec["status"]))
	assert.JSONEq(t, `"`+albARN+`"`, string(rec["arn"]))
	assert.JSONEq(t, `"2.11.0"`, string(rec["addonVersion"]))

	_, manifest := rawJSONObject(t, kv, albManifestKey)
	assert.JSONEq(t, `{"addonName":"aws-load-balancer-controller","addonVersion":"2.11.0",`+
		`"serviceAccountRoleArn":"`+addonLifecycleRoleARN+`","configurationValues":"{\"replicaCount\":2}"}`,
		string(manifest))
}

// Health is persisted only on failure and projected as a single message-only
// issue; a pre-existing blob must keep decoding after the record moves.
func TestAddonRecord_DecodesHandWrittenJSON(t *testing.T) {
	t.Parallel()
	svc, kv := setupStagingAddonService(t)
	_, err := kv.Put(t.Context(), albRecordKey, []byte(`{
		"addonName":"aws-load-balancer-controller","addonVersion":"2.11.0","status":"DEGRADED",
		"serviceAccountRoleArn":"`+addonLifecycleRoleARN+`","health":"rollout stalled",
		"arn":"`+albARN+`","tags":{"team":"platform"},
		"createdAt":"2026-06-08T00:00:00Z","modifiedAt":"2026-06-09T00:00:00Z"}`))
	require.NoError(t, err)

	desc, err := svc.DescribeAddon(context.Background(), &eks.DescribeAddonInput{
		ClusterName: aws.String("c1"), AddonName: aws.String(albController),
	}, testAccountID)
	require.NoError(t, err)
	a := desc.Addon
	assert.Equal(t, eks.AddonStatusDegraded, aws.StringValue(a.Status))
	assert.Equal(t, albARN, aws.StringValue(a.AddonArn))
	assert.Equal(t, addonLifecycleRoleARN, aws.StringValue(a.ServiceAccountRoleArn))
	assert.Equal(t, "platform", aws.StringValue(a.Tags["team"]))
	assert.Equal(t, time.Date(2026, 6, 9, 0, 0, 0, 0, time.UTC), aws.TimeValue(a.ModifiedAt))
	require.NotNil(t, a.Health)
	require.Len(t, a.Health.Issues, 1)
	assert.Equal(t, "rollout stalled", aws.StringValue(a.Health.Issues[0].Message))
	assert.Nil(t, a.Health.Issues[0].Code)
}

// Observed: the token is ignored, so an SDK retry of a create that already
// succeeded is refused rather than returning the existing add-on.
func TestCreateAddon_RepeatedClientRequestTokenIsResourceInUse(t *testing.T) {
	t.Parallel()
	svc, _ := setupStagingAddonService(t)
	in := &eks.CreateAddonInput{
		ClusterName: aws.String("c1"), AddonName: aws.String(albController),
		ClientRequestToken: aws.String("tok-1"),
	}
	_, err := svc.CreateAddon(context.Background(), in, testAccountID)
	require.NoError(t, err)

	_, err = svc.CreateAddon(context.Background(), in, testAccountID)
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorEKSResourceInUse, awserrors.ValidErrorCodeFromError(err))
}

// The installer error is returned raw, the record stays durable as CREATE_FAILED
// with the cause as health, and a retried create is refused.
func TestCreateAddon_InstallerFailureLeavesDurableCreateFailedRecord(t *testing.T) {
	t.Parallel()
	svc, fake := setupAddonService(t)
	cause := errors.New("delivery bus unreachable")
	fake.installErr = cause

	_, err := svc.CreateAddon(context.Background(), &eks.CreateAddonInput{
		ClusterName: aws.String("c1"), AddonName: aws.String(albController),
	}, testAccountID)
	require.ErrorIs(t, err, cause)

	rec, err := addon.Get(t.Context(), acctKVForTest(t, svc), "c1", albController)
	require.NoError(t, err)
	assert.Equal(t, addon.StatusCreateFailed, rec.Status)
	assert.Equal(t, "delivery bus unreachable", rec.Health)

	fake.installErr = nil
	_, err = svc.CreateAddon(context.Background(), &eks.CreateAddonInput{
		ClusterName: aws.String("c1"), AddonName: aws.String(albController),
	}, testAccountID)
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorEKSResourceInUse, awserrors.ValidErrorCodeFromError(err))
	assert.Len(t, fake.installs, 1, "the refused retry must not re-drive the installer")
}

// Observed: a failed re-stage on update marks the add-on CREATE_FAILED, the
// same as a failed create; there is no UPDATE_FAILED state.
func TestUpdateAddon_InstallerFailureMarksCreateFailed(t *testing.T) {
	t.Parallel()
	svc, fake := setupAddonService(t)
	_, err := svc.CreateAddon(context.Background(), &eks.CreateAddonInput{
		ClusterName: aws.String("c1"), AddonName: aws.String("argocd"),
	}, testAccountID)
	require.NoError(t, err)

	cause := errors.New("delivery bus unreachable")
	fake.installErr = cause
	_, err = svc.UpdateAddon(context.Background(), &eks.UpdateAddonInput{
		ClusterName: aws.String("c1"), AddonName: aws.String("argocd"),
		ConfigurationValues: aws.String(`{"replicaCount":2}`),
	}, testAccountID)
	require.ErrorIs(t, err, cause)

	rec, err := addon.Get(t.Context(), acctKVForTest(t, svc), "c1", "argocd")
	require.NoError(t, err)
	assert.Equal(t, addon.StatusCreateFailed, rec.Status)
	assert.Equal(t, `{"replicaCount":2}`, rec.ConfigurationValues, "the new desired config stays stored")
}

// Observed conflict, not desired behaviour: the API reports the update
// Successful at staging time, keyed by the add-on ARN, while the durable record
// remains UPDATING until a guest report arrives.
func TestUpdateAddon_ReportsSuccessfulWhileRecordIsUpdating(t *testing.T) {
	t.Parallel()
	svc, kv := setupStagingAddonService(t)
	_, err := svc.CreateAddon(context.Background(), &eks.CreateAddonInput{
		ClusterName: aws.String("c1"), AddonName: aws.String(albController),
	}, testAccountID)
	require.NoError(t, err)

	update := func() *eks.Update {
		out, uerr := svc.UpdateAddon(context.Background(), &eks.UpdateAddonInput{
			ClusterName: aws.String("c1"), AddonName: aws.String(albController),
			ServiceAccountRoleArn: aws.String(addonLifecycleRoleARN),
			ConfigurationValues:   aws.String(`{"replicaCount":3}`),
			ResolveConflicts:      aws.String(eks.ResolveConflictsPreserve),
		}, testAccountID)
		require.NoError(t, uerr)
		return out.Update
	}
	first := update()
	assert.Equal(t, eks.UpdateStatusSuccessful, aws.StringValue(first.Status))
	assert.Equal(t, eks.UpdateTypeAddonUpdate, aws.StringValue(first.Type))
	assert.Equal(t, albARN, aws.StringValue(first.Id))
	assert.Empty(t, first.Params)

	rec, _ := rawJSONObject(t, kv, albRecordKey)
	assert.JSONEq(t, `"UPDATING"`, string(rec["status"]))
	_, manifest := rawJSONObject(t, kv, albManifestKey)
	assert.JSONEq(t, `{"addonName":"aws-load-balancer-controller","addonVersion":"2.11.0",`+
		`"serviceAccountRoleArn":"`+addonLifecycleRoleARN+`","configurationValues":"{\"replicaCount\":3}"}`,
		string(manifest))

	second := update()
	assert.Equal(t, aws.StringValue(first.Id), aws.StringValue(second.Id), "update IDs are not unique per update")
}

// Observed conflict, not desired behaviour: report.Version is never compared
// with the desired version, so a report for an older version settles the
// current desired generation.
func TestAddonStatusReport_MismatchedVersionIsAccepted(t *testing.T) {
	t.Parallel()
	_, kv := setupStagingAddonService(t)
	r := &ClusterReconciler{acctKV: kv, clusterName: "c1", addonReports: addon.New("", nil)}
	put := func(status addon.Status) {
		now := time.Now().UTC()
		require.NoError(t, putAddonRecord(t, kv, "c1", &addon.Record{
			AddonName: "argocd", AddonVersion: "3.0.23", Status: status, Arn: "arn", CreatedAt: now, ModifiedAt: now,
		}))
	}
	get := func() *addon.Record {
		rec, err := addon.Get(t.Context(), kv, "c1", "argocd")
		require.NoError(t, err)
		return rec
	}
	put(addon.StatusUpdating)
	r.applyAddonStatusReport(t.Context(), eksv1.AddonStatusReport{Addon: "argocd", Version: "1.0.0", Phase: eksv1.AddonPhaseReady, TS: 1})
	assert.Equal(t, addon.StatusActive, get().Status)
	assert.Equal(t, "3.0.23", get().AddonVersion, "the report's version is not recorded either")

	put(addon.StatusUpdating)
	r.applyAddonStatusReport(t.Context(), eksv1.AddonStatusReport{Addon: "argocd", Version: "1.0.0", Phase: eksv1.AddonPhaseFailed, Message: "old", TS: 1})
	assert.Equal(t, addon.StatusCreateFailed, get().Status)
	assert.Equal(t, "old", get().Health)

	// A later ready report lifts CREATE_FAILED straight back to ACTIVE; failure
	// is not terminal against reports.
	r.applyAddonStatusReport(t.Context(), eksv1.AddonStatusReport{Addon: "argocd", Version: "1.0.0", Phase: eksv1.AddonPhaseReady, TS: 0})
	assert.Equal(t, addon.StatusActive, get().Status)
	assert.Empty(t, get().Health)
}

// Observed conflict, not desired behaviour: DELETING exists only in the
// response. Record and manifest are gone before any guest removal is observed,
// and preserve does not change that.
func TestDeleteAddon_ReturnsDeletingAfterErasingRecordAndManifest(t *testing.T) {
	t.Parallel()
	svc, kv := setupStagingAddonService(t)
	_, err := svc.CreateAddon(context.Background(), &eks.CreateAddonInput{
		ClusterName: aws.String("c1"), AddonName: aws.String(albController),
	}, testAccountID)
	require.NoError(t, err)

	out, err := svc.DeleteAddon(context.Background(), &eks.DeleteAddonInput{
		ClusterName: aws.String("c1"), AddonName: aws.String(albController), Preserve: aws.Bool(true),
	}, testAccountID)
	require.NoError(t, err)
	assert.Equal(t, eks.AddonStatusDeleting, aws.StringValue(out.Addon.Status))
	assert.Equal(t, albARN, aws.StringValue(out.Addon.AddonArn))

	for _, key := range []string{albRecordKey, albManifestKey} {
		_, gerr := kv.Get(t.Context(), key)
		require.ErrorIs(t, gerr, jetstream.ErrKeyNotFound, "key %s", key)
	}
	staged, err := svc.ListStagedAddonManifests(context.Background(), &ListStagedAddonManifestsInput{ClusterName: "c1"}, testAccountID)
	require.NoError(t, err)
	assert.Empty(t, staged.Manifests)

	// A report racing the delete does not recreate the record.
	r := &ClusterReconciler{acctKV: kv, clusterName: "c1", addonReports: addon.New("", nil)}
	r.applyAddonStatusReport(t.Context(), eksv1.AddonStatusReport{Addon: albController, Version: "2.11.0", Phase: eksv1.AddonPhaseReady})
	_, err = addon.Get(t.Context(), kv, "c1", albController)
	require.ErrorIs(t, err, addon.ErrNotFound)

	_, err = svc.DeleteAddon(context.Background(), &eks.DeleteAddonInput{
		ClusterName: aws.String("c1"), AddonName: aws.String(albController),
	}, testAccountID)
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorEKSResourceNotFound, awserrors.ValidErrorCodeFromError(err))
}

// Add-on actions check only that the cluster meta exists, not its status.
func TestAddonActionsIgnoreClusterLifecycleStatus(t *testing.T) {
	t.Parallel()
	svc, kv := setupStagingAddonService(t)
	require.NoError(t, PutClusterMeta(t.Context(), kv, &ClusterMeta{Name: "c1", Status: ClusterStatusDeleting}))

	out, err := svc.CreateAddon(context.Background(), &eks.CreateAddonInput{
		ClusterName: aws.String("c1"), AddonName: aws.String("argocd"),
	}, testAccountID)
	require.NoError(t, err)
	assert.Equal(t, eks.AddonStatusCreating, aws.StringValue(out.Addon.Status))
}

// Cluster delete erases add-ons only through the prefix purge, record and
// manifest alike, with no per-add-on delete; a sibling cluster's add-on stays.
func TestDeleteClusterPrefix_SweepsAddonRecordsAndManifestsScopedToCluster(t *testing.T) {
	t.Parallel()
	svc, kv := setupStagingAddonService(t)
	seedTestCluster(t, svc, "c2")
	for _, cluster := range []string{"c1", "c2"} {
		_, err := svc.CreateAddon(context.Background(), &eks.CreateAddonInput{
			ClusterName: aws.String(cluster), AddonName: aws.String(albController),
		}, testAccountID)
		require.NoError(t, err)
	}

	require.NoError(t, DeleteClusterPrefix(t.Context(), kv, "c1"))

	for _, key := range []string{albRecordKey, albManifestKey} {
		_, err := kv.Get(t.Context(), key)
		require.ErrorIs(t, err, jetstream.ErrKeyNotFound, "key %s", key)
	}
	for _, key := range []string{addon.Key("c2", albController), addon.ManifestKey("c2", albController)} {
		_, err := kv.Get(t.Context(), key)
		require.NoError(t, err, "key %s", key)
	}
}
