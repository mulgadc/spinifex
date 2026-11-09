package vm

import (
	"bytes"
	"compress/gzip"
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/spinifex/domains/ec2/instance/uefistore"
	"github.com/mulgadc/spinifex/spinifex/foundation/state/kvstore"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newInstance is a UEFI instance RunInstances has just created.
func (f *uefiFixture) newInstance() *VM {
	return &VM{
		ID: testUEFIInstance, AccountID: "111122223333", InstanceType: "t3.nano", BootMode: "uefi",
		Status: StatePending, FirstLaunch: true, Instance: &ec2.Instance{},
	}
}

func (f *uefiFixture) objectKeys(t *testing.T) []string {
	t.Helper()
	keys, err := uefistore.ListObjects(t.Context(), f.objects, testUEFIBucket, testUEFIInstance)
	require.NoError(t, err)
	return keys
}

func gunzip(t *testing.T, body []byte) []byte {
	t.Helper()
	zr, err := gzip.NewReader(bytes.NewReader(body))
	require.NoError(t, err)
	out, err := io.ReadAll(zr)
	require.NoError(t, err)
	return out
}

// Seeding takes the absent claim, records the store live at the claim's
// generation, puts the template at a fresh key and points the record at it;
// the launch that follows boots the local copy without a get.
func TestSeedUEFIVarStore_ClaimsRecordsAndPointsAtTheTemplate(t *testing.T) {
	f := newUEFIFixture(t)
	instance := f.newInstance()

	require.NoError(t, f.m.SeedUEFIVarStore(t.Context(), instance))

	holder, held := f.claimHolder(t)
	require.True(t, held)
	assert.Equal(t, "node-a", holder.Holder)
	rec, _ := f.record(t)
	assert.Equal(t, uefistore.StateLive, rec.State)
	assert.Equal(t, holder.Generation, rec.Generation)
	require.NotNil(t, rec.Pointer)
	assert.Equal(t, []string{rec.Pointer.Key}, f.objectKeys(t))
	assert.Equal(t, f.identity, rec.Pointer.Firmware)
	assert.Equal(t, "node-a", rec.Pointer.WriterNode)
	assert.Equal(t, make([]byte, testUEFIVarsSize), gunzip(t, f.objectBody(t, rec.Pointer.Key)), "the object is the template region")

	f.m.deps.UEFIVarStore.Objects = &failingGetStore{MemoryObjectStore: f.objects, err: assert.AnError}
	got, err := f.prepare(t, instance)
	require.NoError(t, err)
	assert.True(t, got.reused, "the launch boots the seeded copy without a get")
}

// A BIOS guest has no store, so seeding touches nothing.
func TestSeedUEFIVarStore_BIOSGuestHasNoStore(t *testing.T) {
	f := newUEFIFixture(t)
	instance := f.newInstance()
	instance.BootMode = "legacy-bios"

	require.NoError(t, f.m.SeedUEFIVarStore(t.Context(), instance))

	_, held := f.claimHolder(t)
	assert.False(t, held)
	_, _, err := f.records.Get(t.Context(), testUEFIInstance)
	require.ErrorIs(t, err, kvstore.ErrNotFound)
	assert.Empty(t, f.objectKeys(t))
}

// recordsTerminator is the daemon's terminator over the fixture's records.
type recordsTerminator struct{ f *uefiFixture }

func (r recordsTerminator) MarkUEFIStoreDeleting(id string) (bool, error) {
	return r.f.records.MarkDeleting(context.Background(), id)
}

func (r recordsTerminator) RemoveLocalUEFIStore(id string) error {
	dir, err := uefiInstanceDir(r.f.dataDir, id)
	if err != nil {
		return err
	}
	return os.RemoveAll(dir)
}

// withLifecycle wires what MarkFailed and terminate need on top of the store.
func (f *uefiFixture) withLifecycle(t *testing.T) *recordedTransitions {
	t.Helper()
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	rt := (&recordedTransitions{}).bind(f.m)
	deps := f.m.deps
	deps.StateStore = newFakeStateStore()
	deps.TransitionState = rt.apply
	deps.ShutdownSignal = func() bool { return false }
	deps.UEFIStores = recordsTerminator{f: f}
	f.m.SetDeps(deps)
	return rt
}

func TestSeedUEFIVarStore_KeepsNoLocalCopyWhenTheDirectoryIsUnusable(t *testing.T) {
	f := newUEFIFixture(t)
	require.NoError(t, os.WriteFile(filepath.Join(f.dataDir, "uefi"), nil, 0o600))

	require.NoError(t, f.m.SeedUEFIVarStore(t.Context(), f.newInstance()), "the object is the store; the copy only saves a get")

	rec, _ := f.record(t)
	require.NotNil(t, rec.Pointer)
}
