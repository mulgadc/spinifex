package accountcleanup

//test:in-package — the storage owners are fakes so each test controls the
// read or delete that fails and can assert no delete happened.

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/spinifex/domains/ec2/accountinventory"
	ec2instance "github.com/mulgadc/spinifex/spinifex/domains/ec2/instance"
	ec2spotinstance "github.com/mulgadc/spinifex/spinifex/domains/ec2/spotinstance"
	awserrors "github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	freeVol     = "vol-0aaaaaaaaaaaaaaa1"
	attachedVol = "vol-0aaaaaaaaaaaaaaa2"
	mappedVol   = "vol-0aaaaaaaaaaaaaaa3"
	deletingVol = "vol-0aaaaaaaaaaaaaaa4"
	driftedVol  = "vol-0aaaaaaaaaaaaaaa5"
)

type fakeVolumes struct {
	vols []accountinventory.VolumeItem
	// failAt makes the nth read (1-based) fail; 0 means never.
	failAt int
	reads  int
}

func (f *fakeVolumes) VolumeInventory(context.Context, string) ([]accountinventory.VolumeItem, error) {
	f.reads++
	if f.failAt == f.reads || f.failAt < 0 {
		return nil, errors.New("volume store unreadable")
	}
	return slices.Clone(f.vols), nil
}

// fakeDeleter acts on the volume fake: reclaim "gone" removes the record,
// "deleting" marks it, anything else leaves it.
type fakeDeleter struct {
	store    *fakeVolumes
	reclaim  string
	errFor   map[string]error
	deleted  []string
	onDelete func()
}

func (f *fakeDeleter) DeleteVolume(_ context.Context, _, id string) error {
	if err := f.errFor[id]; err != nil {
		return err
	}
	f.deleted = append(f.deleted, id)
	if f.onDelete != nil {
		f.onDelete()
	}
	f.store.vols = slices.DeleteFunc(f.store.vols, func(v accountinventory.VolumeItem) bool {
		if v.VolumeID != id {
			return false
		}
		return f.reclaim == "gone"
	})
	if f.reclaim == "deleting" {
		for i := range f.store.vols {
			if f.store.vols[i].VolumeID == id {
				f.store.vols[i].State = ec2.VolumeStateDeleting
			}
		}
	}
	return nil
}

type fakeSpotScan struct {
	scan *ec2spotinstance.AccountScan
	err  error
}

func (f *fakeSpotScan) ScanAccount(context.Context, string) (*ec2spotinstance.AccountScan, error) {
	return f.scan, f.err
}

type fakeTags struct {
	ids       []string
	scanErr   error
	rescanErr error
	deleteErr map[string]error
	scans     int
	deleted   []string
	keep      map[string]bool // ids a delete does not actually remove
}

func (f *fakeTags) ScanAccountResourceIDs(context.Context, string, ...string) ([]string, error) {
	f.scans++
	if f.scanErr != nil {
		return nil, f.scanErr
	}
	if f.scans > 1 && f.rescanErr != nil {
		return nil, f.rescanErr
	}
	return slices.Clone(f.ids), nil
}

func (f *fakeTags) DeleteAllTags(_ context.Context, _, id string) error {
	if err := f.deleteErr[id]; err != nil {
		return err
	}
	f.deleted = append(f.deleted, id)
	if !f.keep[id] {
		f.ids = slices.DeleteFunc(f.ids, func(s string) bool { return s == id })
	}
	return nil
}

func volume(id, state, attached string) accountinventory.VolumeItem {
	return accountinventory.VolumeItem{VolumeID: id, State: state, AttachedInstance: attached}
}

type storageRig struct {
	volumes   *fakeVolumes
	deleter   *fakeDeleter
	instances *fakeInventory
	teardown  *fakeTeardown
	spot      *fakeSpotScan
	tags      *fakeTags
}

func newStorageRig() *storageRig {
	store := &fakeVolumes{vols: []accountinventory.VolumeItem{volume(freeVol, ec2.VolumeStateAvailable, "")}}
	return &storageRig{
		volumes:   store,
		deleter:   &fakeDeleter{store: store, reclaim: "gone"},
		instances: &fakeInventory{out: listing(nil)},
		teardown:  &fakeTeardown{},
		spot:      &fakeSpotScan{scan: &ec2spotinstance.AccountScan{}},
		tags:      &fakeTags{},
	}
}

func (r *storageRig) cleanup() *StorageCleanup {
	return NewStorage(r.volumes, r.deleter, r.instances, r.teardown, r.spot, r.tags)
}

func (r *storageRig) run(t *testing.T, force bool) *StorageResult {
	t.Helper()
	res, err := r.cleanup().CleanupStorage(t.Context(), &StorageRequest{AccountID: testAccount, Force: force})
	require.NoError(t, err)
	return res
}

func mapping(volumeID string) *ec2.Instance {
	return &ec2.Instance{
		InstanceId:          aws.String(runningID),
		State:               &ec2.InstanceState{Name: aws.String(ec2.InstanceStateNameRunning)},
		BlockDeviceMappings: []*ec2.InstanceBlockDeviceMapping{{Ebs: &ec2.EbsInstanceBlockDevice{VolumeId: aws.String(volumeID)}}},
	}
}

func TestCleanupStorage_DeletesFreeVolume(t *testing.T) {
	rig := newStorageRig()
	res := rig.run(t, false)

	assert.Equal(t, []string{freeVol}, rig.deleter.deleted)
	assert.Equal(t, DispositionDeleted, itemFor(res, freeVol).Disposition)
	assert.Equal(t, StatusClean, res.Status)
}

// ADR-0003:S7. A read the run cannot complete deletes nothing.
func TestS7_CleanupStorageUnprovenReadDeletesNothing(t *testing.T) {
	cases := map[string]func(*storageRig){
		"volume inventory error": func(r *storageRig) { r.volumes.failAt = -1 },
		"instance inventory error": func(r *storageRig) {
			r.instances.err = errors.New("incomplete listing")
		},
		"unidentified instance entry": func(r *storageRig) {
			r.instances.out = &ec2.DescribeInstancesOutput{Reservations: []*ec2.Reservation{{Instances: []*ec2.Instance{nil}}}}
		},
	}
	for name, arrange := range cases {
		for _, force := range []bool{false, true} {
			t.Run(name, func(t *testing.T) {
				rig := newStorageRig()
				arrange(rig)

				res := rig.run(t, force)

				assert.Empty(t, rig.deleter.deleted, "ADR-0003:S7: an unproven read must delete nothing (force=%v)", force)
				assert.Equal(t, StatusIncomplete, res.Status)
				assert.NotEmpty(t, res.IncompleteReasons)
			})
		}
	}
}

// ADR-0003:S7. A volume with an attachment, a live mapping or an unproven
// state is never deleted, whatever Force says. The drifted record has no state
// at all, as a running root volume's can.
func TestS7_CleanupStorageHeldVolumeIsNeverDeleted(t *testing.T) {
	for _, force := range []bool{false, true} {
		rig := newStorageRig()
		rig.volumes.vols = []accountinventory.VolumeItem{
			volume(attachedVol, ec2.VolumeStateInUse, runningID),
			volume(mappedVol, ec2.VolumeStateAvailable, ""),
			volume(driftedVol, "", ""),
		}
		rig.instances.out = &ec2.DescribeInstancesOutput{Reservations: []*ec2.Reservation{{Instances: []*ec2.Instance{mapping(mappedVol)}}}}

		res := rig.run(t, force)

		assert.Empty(t, rig.deleter.deleted, "ADR-0003:S7: a held volume must not be deleted (force=%v)", force)
		for _, id := range []string{attachedVol, mappedVol} {
			assert.Equal(t, ReasonHolderPresent, itemFor(res, id).Reason, id)
		}
		assert.Equal(t, ReasonVolumeStateUnproven, itemFor(res, driftedVol).Reason)
		assert.Equal(t, StatusIncomplete, res.Status)
	}
}

// A mapping on a terminated instance is stale; it does not hold the volume.
func TestCleanupStorage_TerminatedMappingDoesNotHold(t *testing.T) {
	rig := newStorageRig()
	stale := mapping(freeVol)
	stale.State.Name = aws.String(ec2.InstanceStateNameTerminated)
	rig.instances.out = &ec2.DescribeInstancesOutput{Reservations: []*ec2.Reservation{{Instances: []*ec2.Instance{stale}}}}

	rig.run(t, false)

	assert.Equal(t, []string{freeVol}, rig.deleter.deleted)
}

// The delete path re-checks the holder; its refusal is a held volume, not a failure.
func TestCleanupStorage_OwnerRefusalIsHolderPresent(t *testing.T) {
	rig := newStorageRig()
	rig.deleter.errFor = map[string]error{freeVol: errors.New(awserrors.ErrorVolumeInUse)}

	res := rig.run(t, false)

	assert.Equal(t, ReasonHolderPresent, itemFor(res, freeVol).Reason)
	assert.Equal(t, DispositionBlocked, itemFor(res, freeVol).Disposition)

	rig = newStorageRig()
	rig.deleter.errFor = map[string]error{freeVol: errors.New("store down")}
	res = rig.run(t, false)
	assert.Equal(t, DispositionFailed, itemFor(res, freeVol).Disposition)
	assert.Equal(t, StatusIncomplete, res.Status)
}

func TestCleanupStorage_DeletingVolumeIsPendingAndNotRedeleted(t *testing.T) {
	rig := newStorageRig()
	rig.volumes.vols = []accountinventory.VolumeItem{volume(deletingVol, ec2.VolumeStateDeleting, "")}

	res := rig.run(t, false)

	assert.Empty(t, rig.deleter.deleted)
	assert.Equal(t, DispositionPending, itemFor(res, deletingVol).Disposition)
	assert.Equal(t, ReasonVolumeDeleting, itemFor(res, deletingVol).Reason)
	assert.Len(t, res.Items, 1, "one outcome for the volume, not a second blocked entry")
	assert.Equal(t, StatusIncomplete, res.Status)
}

// ADR-0003:S8. The call returns with a pending outcome while the owner
// reclaims; it re-reads once and does not poll for the record to go.
func TestS8_CleanupStorageReturnsWhileReclaimPending(t *testing.T) {
	rig := newStorageRig()
	rig.deleter.reclaim = "deleting"

	res := rig.run(t, false)

	assert.Equal(t, DispositionPending, itemFor(res, freeVol).Disposition)
	assert.Equal(t, StatusIncomplete, res.Status)
	assert.Equal(t, []string{freeVol}, rig.deleter.deleted, "deleted once, not retried inside the call")
	assert.Equal(t, 3, rig.volumes.reads, "before, after the delete, and for tag ownership; no polling")
}

func TestCleanupStorage_RereadFailureKeepsDeletePending(t *testing.T) {
	rig := newStorageRig()
	rig.volumes.failAt = 2

	res := rig.run(t, false)

	assert.Equal(t, DispositionPending, itemFor(res, freeVol).Disposition)
	assert.Equal(t, ReasonAbsenceUnproven, itemFor(res, freeVol).Reason)
	assert.Contains(t, res.IncompleteReasons, ReasonVolumeInventoryIncomplete)
}

func TestCleanupStorage_ClearsTagResidueOfMissingOwners(t *testing.T) {
	rig := newStorageRig()
	rig.volumes.vols = nil
	rig.spot.scan = &ec2spotinstance.AccountScan{Terminal: []ec2spotinstance.AccountSpotRequest{{RequestID: "sir-closed", State: ec2.SpotInstanceStateClosed}}}
	rig.tags.ids = []string{"i-0gone", "vol-0gone", "sir-0gone", "sir-closed"}

	res := rig.run(t, false)

	assert.ElementsMatch(t, []string{"i-0gone", "vol-0gone", "sir-0gone"}, rig.tags.deleted)
	for _, id := range rig.tags.deleted {
		assert.Equal(t, DispositionDeleted, itemFor(res, id).Disposition)
	}
	assert.Nil(t, itemFor(res, "sir-closed"), "a request that still has a record keeps its tags")
	assert.Equal(t, StatusClean, res.Status)
}

func TestCleanupStorage_KeepsTagsOfResourcesThatStillExist(t *testing.T) {
	rig := newStorageRig()
	rig.volumes.vols = []accountinventory.VolumeItem{volume(attachedVol, ec2.VolumeStateInUse, runningID)}
	rig.instances.out = listing(map[string]string{runningID: ec2.InstanceStateNameRunning})
	rig.teardown.outstanding = nil
	rig.tags.ids = []string{attachedVol, runningID}

	rig.run(t, false)

	assert.Empty(t, rig.tags.deleted)
}

// An owner that cannot be read keeps its entries; absence is not shown.
func TestS7_CleanupStorageUnresolvedOwnerKeepsItsTags(t *testing.T) {
	for _, force := range []bool{false, true} {
		rig := newStorageRig()
		rig.volumes.vols = nil
		rig.volumes.failAt = 3 // the tag-ownership read
		rig.spot.err = errors.New("spot scan failed")
		rig.tags.ids = []string{"vol-0x", "sir-0x"}

		res := rig.run(t, force)

		assert.Empty(t, rig.tags.deleted, "ADR-0003:S7: unresolved owners keep their tag entries (force=%v)", force)
		assert.Contains(t, res.IncompleteReasons, ReasonTagOwnerUnresolved)
		assert.Equal(t, StatusIncomplete, res.Status)
	}
}

func TestCleanupStorage_TagDeleteFailureKeepsRunIncomplete(t *testing.T) {
	rig := newStorageRig()
	rig.volumes.vols = nil
	rig.tags.ids = []string{"vol-0gone"}
	rig.tags.deleteErr = map[string]error{"vol-0gone": errors.New("tag store down")}

	res := rig.run(t, false)

	assert.Equal(t, DispositionFailed, itemFor(res, "vol-0gone").Disposition)
	assert.Equal(t, StatusIncomplete, res.Status)
}

func TestCleanupStorage_TagStillPresentAfterDeleteIsFailed(t *testing.T) {
	rig := newStorageRig()
	rig.volumes.vols = nil
	rig.tags.ids = []string{"vol-0gone"}
	rig.tags.keep = map[string]bool{"vol-0gone": true}

	res := rig.run(t, false)

	assert.Equal(t, DispositionFailed, itemFor(res, "vol-0gone").Disposition)
	assert.Equal(t, ReasonStillPresent, itemFor(res, "vol-0gone").Reason)
	assert.Equal(t, StatusIncomplete, res.Status)
}

func TestCleanupStorage_TagRescanFailureIsPending(t *testing.T) {
	rig := newStorageRig()
	rig.volumes.vols = nil
	rig.tags.ids = []string{"vol-0gone"}
	rig.tags.rescanErr = errors.New("rescan failed")

	res := rig.run(t, false)

	assert.Equal(t, DispositionPending, itemFor(res, "vol-0gone").Disposition)
	assert.Equal(t, StatusIncomplete, res.Status)
}

func TestCleanupStorage_UnreadableTagIndexKeepsRunIncomplete(t *testing.T) {
	rig := newStorageRig()
	rig.tags.scanErr = errors.New("tag index unreadable")

	res := rig.run(t, false)

	assert.Contains(t, res.IncompleteReasons, ReasonTagIndexUnread)
	assert.Equal(t, DispositionDeleted, itemFor(res, freeVol).Disposition, "volumes are not held up by the tag index")
}

func TestCleanupStorage_RejectsBadRequestAndNilDependency(t *testing.T) {
	rig := newStorageRig()
	c := rig.cleanup()
	for _, req := range []*StorageRequest{nil, {}, {AccountID: "123"}} {
		_, err := c.CleanupStorage(t.Context(), req)
		assert.Error(t, err)
	}
	_, err := NewStorage(rig.volumes, rig.deleter, rig.instances, rig.teardown, rig.spot, nil).
		CleanupStorage(t.Context(), &StorageRequest{AccountID: testAccount})
	require.Error(t, err)
	assert.Empty(t, rig.deleter.deleted)
	assert.Zero(t, rig.volumes.reads, "nothing is read before the wiring is proven")
}

// ADR-0003:S7. Unreadable teardown deletes no volume; a terminated record with
// unfinished teardown may still have a QEMU holding a volume no record names.
func TestS7_CleanupStorageUnprovenTeardownDeletesNoVolume(t *testing.T) {
	cases := map[string]func(*storageRig){
		"teardown unreadable": func(r *storageRig) { r.teardown.err = errors.New("store down") },
		"terminated record with failed teardown": func(r *storageRig) {
			r.teardown.outstanding = []ec2instance.OutstandingInstance{{InstanceID: runningID, State: ec2.InstanceStateNameTerminated, Teardown: map[string]string{"qemu": "failed"}}}
		},
	}
	for name, arrange := range cases {
		for _, force := range []bool{false, true} {
			t.Run(name, func(t *testing.T) {
				rig := newStorageRig()
				arrange(rig)

				res := rig.run(t, force)

				assert.Empty(t, rig.deleter.deleted, "ADR-0003:S7: unproven teardown must not allow a volume delete (force=%v)", force)
				assert.Equal(t, StatusIncomplete, res.Status)
			})
		}
	}
}

// A volume that first shows up in the re-read stays visible.
func TestCleanupStorage_VolumeAppearingDuringCleanupIsReported(t *testing.T) {
	rig := newStorageRig()
	rig.deleter.reclaim = "gone"
	rig.deleter.onDelete = func() {
		rig.volumes.vols = append(rig.volumes.vols, volume(deletingVol, ec2.VolumeStateDeleting, ""))
	}

	res := rig.run(t, false)

	item := itemFor(res, deletingVol)
	require.NotNil(t, item)
	assert.Equal(t, DispositionPending, item.Disposition)
	assert.Equal(t, ReasonAppearedDuringCleanup, item.Reason)
	assert.Equal(t, StatusIncomplete, res.Status)
}
