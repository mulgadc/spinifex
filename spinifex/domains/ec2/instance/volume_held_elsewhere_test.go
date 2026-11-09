package instance

//test:in-package — volumeHeldElsewhereError is unexported, and exporting it
//would widen the handler API for a test rather than for a caller.

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/mulgadc/spinifex/contracts/ec2/v1"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/mulgadc/spinifex/spinifex/providers/ebs"
	"github.com/mulgadc/spinifex/spinifex/runtime/compute/vm"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestVolumeHeldElsewhere_IgnoresUnrelatedFailures guards the other direction:
// a genuine internal fault must keep reporting as one rather than being
// relabelled as somebody else's volume.
func TestVolumeHeldElsewhere_IgnoresUnrelatedFailures(t *testing.T) {
	assert.NoError(t, volumeHeldElsewhereError(nil))
	assert.NoError(t, volumeHeldElsewhereError(errors.New("nbd endpoint not ready: timeout waiting for unix socket")))
	assert.NoError(t, volumeHeldElsewhereError(fmt.Errorf("qemu exited: %w", errors.New("no such file"))))

	// A volume whose writes are merely unsealed is opened with a warning rather
	// than refused, so this text is not an exclusion and must not become one.
	assert.NoError(t, volumeHeldElsewhereError(errors.New(
		"took over from node-b, which held unsealed writes since 2026-09-02T17:04:39Z")))

	// Only the provider's code classifies a refusal, so its wording alone does not.
	assert.NoError(t, volumeHeldElsewhereError(errors.New(
		"failed to mount volume: "+ec2v1.VolumeHeldElsewhereText+": held by node-b")))
}

// A provider is free to word its refusal however it likes; the held-elsewhere
// code is what says another node has the volume.
func TestVolumeHeldElsewhere_MatchesTheProviderCodeWhateverTheWording(t *testing.T) {
	refusal := fmt.Errorf("publish volume vol-1: %w", &ebsprovider.ProviderError{
		Code:    ebsprovider.ErrorCodeVolumeHeldElsewhere,
		Message: "volume vol-1 is published to node-a",
	})

	got := volumeHeldElsewhereError(refusal)
	require.Error(t, got, "a held-elsewhere code must be classified even without viperblockd's text")
	assert.ErrorIs(t, got, ec2v1.ErrVolumeHeldElsewhere)
	assert.Contains(t, got.Error(), "node-a", "the caller has to learn where the data is")
	code, _, ok := awserrors.ResolveErrorDetail(got)
	require.True(t, ok)
	assert.Equal(t, awserrors.ErrorIncorrectState, code)
}

// volume_in_use is a read-only mismatch, not another node's claim, so it must
// not be relabelled as a volume held elsewhere even though the sentinels nest.
func TestVolumeHeldElsewhere_VolumeInUseAloneIsNotHeldElsewhere(t *testing.T) {
	mismatch := fmt.Errorf("publish volume vol-1: %w", &ebsprovider.ProviderError{
		Code:    ebsprovider.ErrorCodeVolumeInUse,
		Message: "volume vol-1 is exported read-write, not read-only",
	})
	assert.NoError(t, volumeHeldElsewhereError(mismatch))
}

// heldElsewhereMounter refuses every mount the way the contract mounter
// reports a provider's held-elsewhere code, in wording that is not viperblockd's.
type heldElsewhereMounter struct{ raceVolumeMounter }

func (heldElsewhereMounter) Mount(context.Context, *vm.VM) error {
	return fmt.Errorf("publish volume vol-1: %w", &ebsprovider.ProviderError{
		Code:    ebsprovider.ErrorCodeVolumeHeldElsewhere,
		Message: "volume vol-1 is published to node-a",
	})
}

// Both start paths a caller can reach must hand back the classified refusal:
// the stopped-instance claim (ec2.start) and the live owner (ec2.cmd), which
// the EKS reconciler tries first.
func TestStartPaths_ReportAVolumeHeldElsewhereByCode(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	types, _ := defaultPrepareInstanceTypes()

	t.Run("claimed relaunch", func(t *testing.T) {
		mgr := vm.NewManager()
		mgr.SetDeps(vm.Deps{NodeID: "node-b", VolumeMounter: heldElsewhereMounter{}})
		svc := &InstanceServiceImpl{resourceMgr: &fakeResourceCapacityProvider{instanceTypes: types}, vmMgr: mgr}

		err := svc.launchClaimedInstance(t.Context(), &vm.VM{ID: "i-held-claim", AccountID: "acc", InstanceType: "t3.micro", Status: vm.StatePending}, func() {})
		assert.ErrorIs(t, err, ec2v1.ErrVolumeHeldElsewhere)
		assert.Equal(t, awserrors.ErrorIncorrectState, awserrors.ValidErrorCodeFromError(err))
	})

	t.Run("live owner", func(t *testing.T) {
		const id = "i-held-live"
		mgr := mgrWith(map[string]*vm.VM{id: {ID: id, Status: vm.StateStopped, InstanceType: "t3.micro"}})
		mgr.SetDeps(vm.Deps{NodeID: "node-b", VolumeMounter: heldElsewhereMounter{}})
		v, _ := mgr.Get(id)
		svc := &InstanceServiceImpl{resourceMgr: &fakeResourceCapacityProvider{instanceTypes: types}, vmMgr: mgr}

		err := svc.StartInstance(t.Context(), v, ec2v1.EC2InstanceCommand{ID: id})
		assert.ErrorIs(t, err, ec2v1.ErrVolumeHeldElsewhere)
		assert.Equal(t, awserrors.ErrorIncorrectState, awserrors.ValidErrorCodeFromError(err))
	})
}

// uefiClaimHeldMounter fails the launch the way a start does while a live
// node holds the instance's UEFI variable store.
type uefiClaimHeldMounter struct{ raceVolumeMounter }

func (uefiClaimHeldMounter) Mount(context.Context, *vm.VM) error {
	return fmt.Errorf("%w: node-a holds i-uefi: %w", vm.ErrUEFIClaimHeld, vm.ErrMountRetryable)
}

// A held UEFI store is a capacity refusal on both start paths, so the caller
// tries another node rather than reporting a fault.
func TestStartPaths_ReportAHeldUEFIStoreAsCapacity(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	types, _ := defaultPrepareInstanceTypes()

	t.Run("claimed relaunch", func(t *testing.T) {
		mgr := vm.NewManager()
		mgr.SetDeps(vm.Deps{NodeID: "node-b", VolumeMounter: uefiClaimHeldMounter{}})
		svc := &InstanceServiceImpl{resourceMgr: &fakeResourceCapacityProvider{instanceTypes: types}, vmMgr: mgr}

		err := svc.launchClaimedInstance(t.Context(), &vm.VM{ID: "i-uefi-claim", AccountID: "acc", InstanceType: "t3.micro", Status: vm.StatePending}, func() {})
		assert.Equal(t, awserrors.ErrorInsufficientInstanceCapacity, awserrors.ValidErrorCodeFromError(err))
	})

	t.Run("live owner", func(t *testing.T) {
		const id = "i-uefi-live"
		mgr := mgrWith(map[string]*vm.VM{id: {ID: id, Status: vm.StateStopped, InstanceType: "t3.micro"}})
		mgr.SetDeps(vm.Deps{NodeID: "node-b", VolumeMounter: uefiClaimHeldMounter{}})
		v, _ := mgr.Get(id)
		svc := &InstanceServiceImpl{resourceMgr: &fakeResourceCapacityProvider{instanceTypes: types}, vmMgr: mgr}

		err := svc.StartInstance(t.Context(), v, ec2v1.EC2InstanceCommand{ID: id})
		assert.Equal(t, awserrors.ErrorInsufficientInstanceCapacity, awserrors.ValidErrorCodeFromError(err))
	})
}
