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

	require.NoError(t, f.m.SeedUEFIVarStore(t.Context(), instance, nil))

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

	require.NoError(t, f.m.SeedUEFIVarStore(t.Context(), instance, nil))

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

	require.NoError(t, f.m.SeedUEFIVarStore(t.Context(), f.newInstance(), nil), "the object is the store; the copy only saves a get")

	rec, _ := f.record(t)
	require.NotNil(t, rec.Pointer)
}

const testUEFIImage = "ami-0123456789abcdef0"

// imageStore puts vars as testUEFIImage's store object under fw.
func (f *uefiFixture) imageStore(t *testing.T, vars []byte, fw uefistore.FirmwareIdentity) *uefistore.ImageVarStore {
	t.Helper()
	key := uefistore.KeyPrefix(testUEFIImage) + "image-object"
	f.putObject(t, key, gzipped(t, vars))
	return &uefistore.ImageVarStore{ImageID: testUEFIImage, Key: key, Firmware: fw}
}

// An AMI that names a store this node can boot seeds the new instance's own
// object from it, under the instance's prefix, and the image's object stays.
func TestSeedUEFIVarStore_SeedsFromTheImagesStore(t *testing.T) {
	f := newUEFIFixture(t)
	fw := f.identity
	fw.CodeSHA256 = "an older CODE build"
	image := f.imageStore(t, region("image variables"), fw)
	codeBefore := uefiCounter(t, "mulga.vm.uefi_store.firmware_mismatches", "kind", "code")

	require.NoError(t, f.m.SeedUEFIVarStore(t.Context(), f.newInstance(), image))

	rec, _ := f.record(t)
	require.NotNil(t, rec.Pointer)
	assert.NotEqual(t, image.Key, rec.Pointer.Key, "the instance gets its own object")
	assert.Equal(t, []string{rec.Pointer.Key}, f.objectKeys(t))
	assert.Equal(t, region("image variables"), gunzip(t, f.objectBody(t, rec.Pointer.Key)))
	assert.Equal(t, region("image variables"), f.localVars(t))
	assert.Equal(t, f.identity, rec.Pointer.Firmware, "the seeding node writes the object under its own firmware")
	assert.Equal(t, gzipped(t, region("image variables")), f.objectBody(t, image.Key), "the image's object is left as it was")
	assert.Equal(t, codeBefore+1, uefiCounter(t, "mulga.vm.uefi_store.firmware_mismatches", "kind", "code"),
		"a different CODE build seeds from the image and is counted")
}

// An image store whose arch or VARS size cannot boot here gives way to the
// template, so the launch still boots.
func TestSeedUEFIVarStore_FallsBackToTheTemplateOnAFirmwareMismatch(t *testing.T) {
	for name, mutate := range map[string]func(*uefistore.FirmwareIdentity){
		"arch":      func(fw *uefistore.FirmwareIdentity) { fw.Arch = "arm64" },
		"vars size": func(fw *uefistore.FirmwareIdentity) { fw.VarsSize = 2 * testUEFIVarsSize },
	} {
		t.Run(name, func(t *testing.T) {
			f := newUEFIFixture(t)
			fw := f.identity
			mutate(&fw)
			image := f.imageStore(t, region("image variables"), fw)
			before := uefiCounter(t, "mulga.vm.uefi_store.firmware_mismatches", "kind", "image")

			require.NoError(t, f.m.SeedUEFIVarStore(t.Context(), f.newInstance(), image))

			rec, _ := f.record(t)
			require.NotNil(t, rec.Pointer)
			assert.Equal(t, make([]byte, testUEFIVarsSize), gunzip(t, f.objectBody(t, rec.Pointer.Key)), "the template seeds the store")
			assert.Equal(t, before+1, uefiCounter(t, "mulga.vm.uefi_store.firmware_mismatches", "kind", "image"))
		})
	}
}

// An image store that cannot be read fails the seed before anything is
// claimed or recorded, so the failed launch leaves no store behind.
func TestSeedUEFIVarStore_UnreadableImageStoreRecordsNothing(t *testing.T) {
	f := newUEFIFixture(t)
	image := &uefistore.ImageVarStore{ImageID: testUEFIImage, Key: uefistore.KeyPrefix(testUEFIImage) + "gone", Firmware: f.identity}

	err := f.m.SeedUEFIVarStore(t.Context(), f.newInstance(), image)

	require.ErrorIs(t, err, uefistore.ErrObjectNotFound)
	_, held := f.claimHolder(t)
	assert.False(t, held)
	_, _, err = f.records.Get(t.Context(), testUEFIInstance)
	require.ErrorIs(t, err, kvstore.ErrNotFound)
	assert.Empty(t, f.objectKeys(t))
}
