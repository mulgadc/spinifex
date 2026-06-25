package daemon

import (
	"context"
	"strconv"
	"testing"
	"time"

	handlers_ec2_vpc "github.com/mulgadc/spinifex/spinifex/handlers/ec2/vpc"
	"github.com/mulgadc/spinifex/spinifex/vm"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// reconcilerFixture extends the shared ENI hot-plug fixture with the VM's
// AccountID set (the reconciler skips account-less legacy VMs) and a built
// reconciler whose staleness threshold the test can drive.
func newReconcilerFixture(t *testing.T) (*eniHotPlugFixture, *eniReconciler) {
	t.Helper()
	f := newENIHotPlugFixture(t)
	f.vmInst.AccountID = testAccountID
	r := f.daemon.newENIReconciler()
	require.NotNil(t, r)
	return f, r
}

// seedAttached marks the fixture ENI attached to the VM in KV with the given
// transition state, then optionally materializes the guest device + in-memory
// slot so the reconciler observes a "present" device.
func seedAttached(t *testing.T, f *eniHotPlugFixture, status string, slot int, age time.Duration, detachInFlight, force, seedDevice bool) {
	t.Helper()
	_, err := f.daemon.vpcService.AttachENI(testAccountID, f.eniID, f.vmInst.ID, int64(slot))
	require.NoError(t, err)
	require.NoError(t, f.daemon.vpcService.UpdateENI(testAccountID, f.eniID, func(r *handlers_ec2_vpc.ENIRecord) {
		r.AttachmentStatus = status
		r.HotPlugSlot = slot
		r.DetachInFlight = detachInFlight
		r.DetachForce = force
		r.AttachmentStateAt = time.Now().Add(-age)
	}))
	if seedDevice {
		require.NoError(t, f.stub.DeviceAdd(map[string]any{"id": "net-eni-" + strconv.Itoa(slot)}))
		require.NoError(t, f.stub.NetdevAdd(map[string]any{"id": "hostnet-eni-" + strconv.Itoa(slot)}))
		f.daemon.vmMgr.AdoptENISlot(f.vmInst, f.eniID, slot)
	}
}

func eniStatus(t *testing.T, f *eniHotPlugFixture) handlers_ec2_vpc.ENIRecord {
	t.Helper()
	rec, err := f.daemon.vpcService.GetENIRecord(testAccountID, f.eniID)
	require.NoError(t, err)
	return rec
}

// Row 1: steady attached, device present, in-memory slot lost across restart →
// reconciler re-adopts the slot, reaps nothing, KV unchanged.
func TestENIReconciler_AttachedPresent_ReadoptsSlot(t *testing.T) {
	f, r := newReconcilerFixture(t)
	seedAttached(t, f, "attached", 1, time.Minute, false, false, true)
	f.daemon.vmMgr.ReleaseENISlot(f.vmInst, f.eniID) // simulate slot map lost on restart

	reaped, err := r.Sweep(context.Background())
	require.NoError(t, err)
	assert.Equal(t, 0, reaped)
	assert.Equal(t, "attached", eniStatus(t, f).AttachmentStatus)
	assert.Equal(t, 1, f.daemon.vmMgr.ENISlotForReconcile(f.vmInst, f.eniID))
}

// Row 2: KV attached but the guest no longer exposes the device → detach.
func TestENIReconciler_AttachedAbsent_Detaches(t *testing.T) {
	f, r := newReconcilerFixture(t)
	seedAttached(t, f, "attached", 1, time.Minute, false, false, false)

	reaped, err := r.Sweep(context.Background())
	require.NoError(t, err)
	assert.Equal(t, 1, reaped)
	rec := eniStatus(t, f)
	assert.Equal(t, "available", rec.Status)
	assert.Empty(t, rec.AttachmentStatus)
	assert.Equal(t, 0, rec.HotPlugSlot)
}

// Row 3: attach interrupted, device made it → finish to attached.
func TestENIReconciler_AttachingPresent_Completes(t *testing.T) {
	f, r := newReconcilerFixture(t)
	seedAttached(t, f, "attaching", 1, time.Minute, false, false, true)

	reaped, err := r.Sweep(context.Background())
	require.NoError(t, err)
	assert.Equal(t, 1, reaped)
	assert.Equal(t, "attached", eniStatus(t, f).AttachmentStatus)
}

// Row 4: attach interrupted before the device materialized → roll back.
func TestENIReconciler_AttachingAbsent_RollsBack(t *testing.T) {
	f, r := newReconcilerFixture(t)
	seedAttached(t, f, "attaching", 1, time.Minute, false, false, false)

	reaped, err := r.Sweep(context.Background())
	require.NoError(t, err)
	assert.Equal(t, 1, reaped)
	assert.Equal(t, "available", eniStatus(t, f).Status)
}

// Row 5: detach interrupted, device still present → replay hot-unplug.
func TestENIReconciler_DetachingPresent_ReplaysUnplug(t *testing.T) {
	f, r := newReconcilerFixture(t)
	seedAttached(t, f, "detaching", 1, time.Minute, true, false, true)

	reaped, err := r.Sweep(context.Background())
	require.NoError(t, err)
	assert.Equal(t, 1, reaped)
	assert.False(t, f.stub.HasDevice("net-eni-1"), "device should be removed")
	rec := eniStatus(t, f)
	assert.Equal(t, "available", rec.Status)
	assert.False(t, rec.DetachInFlight)
}

// Row 6: detach interrupted after the device was already gone → finalize KV.
func TestENIReconciler_DetachingAbsent_Finalizes(t *testing.T) {
	f, r := newReconcilerFixture(t)
	seedAttached(t, f, "detaching", 1, time.Minute, true, false, false)

	reaped, err := r.Sweep(context.Background())
	require.NoError(t, err)
	assert.Equal(t, 1, reaped)
	rec := eniStatus(t, f)
	assert.Equal(t, "available", rec.Status)
	assert.False(t, rec.DetachInFlight)
}

// Row 7: a live hot-plug device no KV record claims → removed as an orphan.
func TestENIReconciler_OrphanDevice_Removed(t *testing.T) {
	f, r := newReconcilerFixture(t)
	require.NoError(t, f.stub.DeviceAdd(map[string]any{"id": "net-eni-2"}))
	require.NoError(t, f.stub.NetdevAdd(map[string]any{"id": "hostnet-eni-2"}))

	reaped, err := r.Sweep(context.Background())
	require.NoError(t, err)
	assert.Equal(t, 1, reaped)
	assert.False(t, f.stub.HasDevice("net-eni-2"), "orphan device should be removed")
}

// Row 8: an in-memory slot entry whose KV attachment is gone → slot released.
func TestENIReconciler_OrphanSlot_Released(t *testing.T) {
	f, r := newReconcilerFixture(t)
	f.daemon.vmMgr.AdoptENISlot(f.vmInst, "eni-ghost", 3)

	reaped, err := r.Sweep(context.Background())
	require.NoError(t, err)
	assert.Equal(t, 1, reaped)
	assert.Equal(t, 0, f.daemon.vmMgr.ENISlotForReconcile(f.vmInst, "eni-ghost"))
}

// Staleness gate: a fresh attaching transition is left untouched so the
// reconciler never races a live attach handler.
func TestENIReconciler_FreshAttaching_NotTouched(t *testing.T) {
	f, r := newReconcilerFixture(t)
	seedAttached(t, f, "attaching", 1, time.Second, false, false, false)

	reaped, err := r.Sweep(context.Background())
	require.NoError(t, err)
	assert.Equal(t, 0, reaped)
	assert.Equal(t, "attaching", eniStatus(t, f).AttachmentStatus)
}

// A stopped or account-less VM is skipped entirely.
func TestENIReconciler_SkipsNonRunning(t *testing.T) {
	f, r := newReconcilerFixture(t)
	f.vmInst.Status = vm.StateStopped
	seedAttached(t, f, "attached", 1, time.Minute, false, false, false)

	reaped, err := r.Sweep(context.Background())
	require.NoError(t, err)
	assert.Equal(t, 0, reaped)
	assert.Equal(t, "attached", eniStatus(t, f).AttachmentStatus)
}
