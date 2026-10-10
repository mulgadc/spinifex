package addon

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeInstaller records deliveries so tests can see what the owner staged.
type fakeInstaller struct {
	installs   []string
	uninstalls []string
	installErr error
}

var _ Installer = (*fakeInstaller)(nil)

func (f *fakeInstaller) Install(_ context.Context, _, _ string, rec *Record) error {
	f.installs = append(f.installs, rec.AddonName+"@"+rec.AddonVersion)
	return f.installErr
}

func (f *fakeInstaller) Uninstall(_ context.Context, _, _, addon string) error {
	f.uninstalls = append(f.uninstalls, addon)
	return nil
}

func TestNextStatus(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name        string
		cur         Status
		phase       Phase
		wantStatus  Status
		wantChanged bool
	}{
		{"ready from creating -> active", StatusCreating, PhaseReady, StatusActive, true},
		{"ready from updating -> active", StatusUpdating, PhaseReady, StatusActive, true},
		{"ready when already active -> no-op", StatusActive, PhaseReady, StatusActive, false},
		{"applied is informational -> no-op", StatusCreating, PhaseApplied, StatusCreating, false},
		{"failed from creating -> create_failed", StatusCreating, PhaseFailed, StatusCreateFailed, true},
		{"failed from updating -> create_failed", StatusUpdating, PhaseFailed, StatusCreateFailed, true},
		{"failed when active -> degraded", StatusActive, PhaseFailed, StatusDegraded, true},
		{"failed when degraded -> sticky", StatusDegraded, PhaseFailed, StatusDegraded, false},
		{"failed when create_failed -> sticky", StatusCreateFailed, PhaseFailed, StatusCreateFailed, false},
		{"unknown phase -> no-op", StatusCreating, Phase("bogus"), StatusCreating, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, changed := nextStatus(tc.cur, tc.phase)
			assert.Equal(t, tc.wantStatus, got)
			assert.Equal(t, tc.wantChanged, changed)
		})
	}
}

func TestOwnerCreate_DuplicateReturnsErrExistsWithoutOverwriteOrRestage(t *testing.T) {
	t.Parallel()
	kv := newTestKV(t)
	inst := &fakeInstaller{}
	owner := New("ap-southeast-2", inst)

	first, err := owner.Create(t.Context(), kv, testAccountID, Desired{Cluster: "c1", Name: "argocd", Version: "3.0.23"})
	require.NoError(t, err)
	assert.Equal(t, StatusCreating, first.Status)
	assert.Equal(t, "arn:aws:eks:ap-southeast-2:111122223333:addon/c1/argocd", first.Arn)

	_, err = owner.Create(t.Context(), kv, testAccountID, Desired{
		Cluster: "c1", Name: "argocd", Version: "3.0.23", ConfigurationValues: `{"x":1}`,
	})
	require.ErrorIs(t, err, ErrExists)

	got, err := Get(t.Context(), kv, "c1", "argocd")
	require.NoError(t, err)
	assert.Empty(t, got.ConfigurationValues, "the existing record is left untouched")
	assert.Equal(t, []string{"argocd@3.0.23"}, inst.installs, "the refused create must not re-stage")
}

func TestOwnerCreate_StagingFailureLeavesCreateFailedRecord(t *testing.T) {
	t.Parallel()
	kv := newTestKV(t)
	cause := errors.New("delivery bus unreachable")
	owner := New("us-east-1", &fakeInstaller{installErr: cause})

	_, err := owner.Create(t.Context(), kv, testAccountID, Desired{Cluster: "c1", Name: "argocd", Version: "3.0.23"})
	require.ErrorIs(t, err, cause)

	got, err := Get(t.Context(), kv, "c1", "argocd")
	require.NoError(t, err)
	assert.Equal(t, StatusCreateFailed, got.Status)
	assert.Equal(t, "delivery bus unreachable", got.Health)
}

func TestOwnerUpdateAndDelete_AbsentIsErrNotFound(t *testing.T) {
	t.Parallel()
	kv := newTestKV(t)
	inst := &fakeInstaller{}
	owner := New("us-east-1", inst)

	_, err := owner.Update(t.Context(), kv, testAccountID, "c1", "argocd", Change{Version: "3.0.23"})
	require.ErrorIs(t, err, ErrNotFound)
	_, err = owner.Delete(t.Context(), kv, testAccountID, "c1", "argocd")
	require.ErrorIs(t, err, ErrNotFound)
	assert.Empty(t, inst.installs)
	assert.Empty(t, inst.uninstalls)
}

func TestOwnerApplyReport(t *testing.T) {
	t.Parallel()
	kv := newTestKV(t)
	owner := New("us-east-1", &fakeInstaller{})
	_, err := owner.Create(t.Context(), kv, testAccountID, Desired{Cluster: "c1", Name: "argocd", Version: "3.0.23"})
	require.NoError(t, err)
	get := func() *Record {
		rec, gerr := Get(t.Context(), kv, "c1", "argocd")
		require.NoError(t, gerr)
		return rec
	}

	require.NoError(t, owner.ApplyReport(t.Context(), kv, "c1", Report{Addon: "argocd", Phase: PhaseFailed, Message: "image pull"}))
	assert.Equal(t, StatusCreateFailed, get().Status)
	assert.Equal(t, "image pull", get().Health)

	require.NoError(t, owner.ApplyReport(t.Context(), kv, "c1", Report{Addon: "argocd", Phase: PhaseReady}))
	assert.Equal(t, StatusActive, get().Status)
	assert.Empty(t, get().Health)

	before := get().ModifiedAt
	require.NoError(t, owner.ApplyReport(t.Context(), kv, "c1", Report{Addon: "argocd", Phase: PhaseApplied}))
	assert.Equal(t, before, get().ModifiedAt, "a report that changes nothing writes nothing")

	require.NoError(t, owner.ApplyReport(t.Context(), kv, "c1", Report{Addon: "ghost", Phase: PhaseReady}))
	require.NoError(t, owner.ApplyReport(t.Context(), kv, "c1", Report{Phase: PhaseReady}))
	_, err = Get(t.Context(), kv, "c1", "ghost")
	require.ErrorIs(t, err, ErrNotFound, "a report never creates a record")
}

func TestOwnerEnsureGPUDevicePlugin_StagesDefaultVersionOnceAndKeepsAnyExistingRecord(t *testing.T) {
	t.Parallel()
	kv := newTestKV(t)
	inst := &fakeInstaller{}
	owner := New("us-east-1", inst)

	require.NoError(t, owner.EnsureGPUDevicePlugin(t.Context(), kv, testAccountID, "c1"))
	rec, err := Get(t.Context(), kv, "c1", NvidiaDevicePlugin)
	require.NoError(t, err)
	assert.Equal(t, "0.17.4", rec.AddonVersion)
	assert.Equal(t, StatusCreating, rec.Status)

	// An existing record in any status, including CREATE_FAILED, blocks re-staging.
	require.NoError(t, owner.ApplyReport(t.Context(), kv, "c1", Report{Addon: NvidiaDevicePlugin, Phase: PhaseFailed, Message: "boom"}))
	require.NoError(t, owner.EnsureGPUDevicePlugin(t.Context(), kv, testAccountID, "c1"))
	rec, err = Get(t.Context(), kv, "c1", NvidiaDevicePlugin)
	require.NoError(t, err)
	assert.Equal(t, StatusCreateFailed, rec.Status)
	assert.Equal(t, []string{NvidiaDevicePlugin + "@0.17.4"}, inst.installs)
}

func TestOwnerEnsureGPUDevicePlugin_ReturnsStagingFailure(t *testing.T) {
	t.Parallel()
	kv := newTestKV(t)
	cause := errors.New("delivery bus unreachable")
	owner := New("us-east-1", &fakeInstaller{installErr: cause})

	require.ErrorIs(t, owner.EnsureGPUDevicePlugin(t.Context(), kv, testAccountID, "c1"), cause)
}
