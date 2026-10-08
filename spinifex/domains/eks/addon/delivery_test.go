package addon

import (
	"context"
	"errors"
	"testing"

	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func stagingFor(kv jetstream.KeyValue) *StagingInstaller {
	return NewStagingInstaller(func(context.Context, string) (jetstream.KeyValue, error) { return kv, nil })
}

func TestStagingInstaller_InstallWritesManifestAndUninstallRemovesIt(t *testing.T) {
	t.Parallel()
	kv := newTestKV(t)
	inst := stagingFor(kv)
	rec := sampleRecord("argocd")
	rec.ServiceAccountRoleArn = "arn:aws:iam::111122223333:role/argo"

	require.NoError(t, inst.Install(t.Context(), testAccountID, "c1", rec))
	entry, err := kv.Get(t.Context(), ManifestKey("c1", "argocd"))
	require.NoError(t, err)
	assert.JSONEq(t, `{"addonName":"argocd","addonVersion":"1.0.0","serviceAccountRoleArn":"arn:aws:iam::111122223333:role/argo"}`,
		string(entry.Value()))

	require.NoError(t, inst.Uninstall(t.Context(), testAccountID, "c1", "argocd"))
	_, err = kv.Get(t.Context(), ManifestKey("c1", "argocd"))
	require.ErrorIs(t, err, jetstream.ErrKeyNotFound)
	require.NoError(t, inst.Uninstall(t.Context(), testAccountID, "c1", "argocd"), "uninstalling an absent manifest is not an error")
}

func TestStagingInstaller_SurfacesBucketAndNilRecordErrors(t *testing.T) {
	t.Parallel()
	cause := errors.New("bucket unavailable")
	inst := NewStagingInstaller(func(context.Context, string) (jetstream.KeyValue, error) { return nil, cause })

	require.Error(t, inst.Install(t.Context(), testAccountID, "c1", nil))
	require.ErrorIs(t, inst.Install(t.Context(), testAccountID, "c1", sampleRecord("argocd")), cause)
	require.ErrorIs(t, inst.Uninstall(t.Context(), testAccountID, "c1", "argocd"), cause)
}
