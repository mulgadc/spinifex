package handlers_eks

import (
	"context"
	"testing"

	eksv1 "github.com/mulgadc/spinifex/contracts/eks/v1"
	"github.com/mulgadc/spinifex/spinifex/domains/eks/addon"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The reconciler's subscriber decodes then applies; a report carrying fields
// this binary does not know is applied exactly as one without them.
func TestAddonStatusReport_UnknownFieldsAreAppliedAsToday(t *testing.T) {
	t.Parallel()
	_, kv := setupStagingAddonService(t)
	_, err := kv.Put(t.Context(), "clusters/c1/addons/argocd", []byte(`{"addonName":"argocd","addonVersion":"3.0.23",`+
		`"status":"ACTIVE","arn":"arn","createdAt":"2026-06-08T00:00:00Z","modifiedAt":"2026-06-08T00:00:00Z"}`))
	require.NoError(t, err)
	r := &ClusterReconciler{acctKV: kv, clusterName: "c1", addonReports: addon.New("", nil)}

	report, err := unmarshalAddonStatusReport([]byte(`{"addon":"argocd","version":"3.0.23","phase":"failed",` +
		`"message":"rollout stalled","ts":1,"incarnationId":"stale-inc","generation":1}`))
	require.NoError(t, err)
	r.applyAddonStatusReport(t.Context(), report)

	rec, err := addon.Get(t.Context(), kv, "c1", "argocd")
	require.NoError(t, err)
	assert.Equal(t, addon.StatusDegraded, rec.Status)
	assert.Equal(t, "rollout stalled", rec.Health)
}

// The daemon copies the four v1 manifest fields onto the NATS reply, so a
// staged manifest's unknown fields never reach the gateway or the guest.
func TestListStagedAddonManifests_ProjectsOnlyV1Fields(t *testing.T) {
	t.Parallel()
	svc, kv := setupStagingAddonService(t)
	_, err := kv.Put(t.Context(), albManifestKey, []byte(`{"addonName":"aws-load-balancer-controller",`+
		`"addonVersion":"2.11.0","serviceAccountRoleArn":"`+addonLifecycleRoleARN+`","incarnationId":"inc-1","generation":4}`))
	require.NoError(t, err)

	out, err := svc.ListStagedAddonManifests(context.Background(), &ListStagedAddonManifestsInput{ClusterName: "c1"}, testAccountID)
	require.NoError(t, err)
	assert.Equal(t, []eksv1.StagedAddonManifest{{
		AddonName: "aws-load-balancer-controller", AddonVersion: "2.11.0", ServiceAccountRoleArn: addonLifecycleRoleARN,
	}}, out.Manifests)
}
