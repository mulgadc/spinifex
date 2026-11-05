package accountcleanup

import (
	"errors"
	"testing"

	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/spinifex/domains/ec2/accountinventory"
	ec2instance "github.com/mulgadc/spinifex/spinifex/domains/ec2/instance"
	ec2spotinstance "github.com/mulgadc/spinifex/spinifex/domains/ec2/spotinstance"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestComputeRemainingReportsEveryHeldInstanceAndOpenSpot(t *testing.T) {
	r := newComputeRig()
	r.spot.scans = []*ec2spotinstance.AccountScan{{
		Active:   []ec2spotinstance.AccountSpotRequest{{RequestID: "sir-0aaaaaaaaaaaaaaa1", State: ec2.SpotInstanceStateOpen}},
		Terminal: []ec2spotinstance.AccountSpotRequest{{RequestID: "sir-0aaaaaaaaaaaaaaa2", State: ec2.SpotInstanceStateClosed}},
	}}
	r.teardown.outstanding = []ec2instance.OutstandingInstance{{InstanceID: "i-0aaaaaaaaaaaaaa99", State: ec2.InstanceStateNameTerminated}}

	res, err := r.cleanup().ComputeRemaining(t.Context(), testAccount)
	require.NoError(t, err)

	assert.Equal(t, StatusIncomplete, res.Status)
	assert.NotNil(t, itemFor(res, runningID))
	assert.NotNil(t, itemFor(res, stoppedID))
	assert.NotNil(t, itemFor(res, "i-0aaaaaaaaaaaaaa99"))
	assert.NotNil(t, itemFor(res, "sir-0aaaaaaaaaaaaaaa1"))
	assert.Nil(t, itemFor(res, "sir-0aaaaaaaaaaaaaaa2"), "a closed request launches nothing")
	assert.Empty(t, r.terminator.terminated, "a re-read changes nothing")
}

func TestComputeRemainingCleanWhenNothingHeld(t *testing.T) {
	r := newComputeRig()
	r.inventory.out = listing(nil)
	res, err := r.cleanup().ComputeRemaining(t.Context(), testAccount)
	require.NoError(t, err)
	assert.Equal(t, StatusClean, res.Status)
}

func TestComputeRemainingUnreadStoreIsIncompleteNeverClean(t *testing.T) {
	for name, mutate := range map[string]func(*computeRig){
		"instances": func(r *computeRig) { r.inventory.out, r.inventory.err = nil, errors.New("node silent") },
		"spot":      func(r *computeRig) { r.spot.scanErr = errors.New("spot unreadable") },
		"teardown":  func(r *computeRig) { r.teardown.err = errors.New("no responder") },
	} {
		t.Run(name, func(t *testing.T) {
			r := newComputeRig()
			r.inventory.out = listing(nil)
			mutate(r)
			res, err := r.cleanup().ComputeRemaining(t.Context(), testAccount)
			require.NoError(t, err)
			assert.Equal(t, StatusIncomplete, res.Status)
			assert.NotEmpty(t, res.IncompleteReasons)
		})
	}
}

func TestStorageRemainingReportsVolumeRecordsAndTagResidue(t *testing.T) {
	r := newStorageRig()
	r.volumes.vols = []accountinventory.VolumeItem{
		volume(freeVol, ec2.VolumeStateAvailable, ""),
		volume("vol-0aaaaaaaaaaaaaaa2", ec2.VolumeStateDeleting, ""),
	}
	r.tags.ids = []string{"i-0aaaaaaaaaaaaaaaf", freeVol}

	res, err := r.cleanup().StorageRemaining(t.Context(), testAccount)
	require.NoError(t, err)

	assert.Equal(t, StatusIncomplete, res.Status)
	assert.Equal(t, ReasonRecordRemains, itemFor(res, freeVol).Reason)
	assert.Equal(t, ReasonVolumeDeleting, itemFor(res, "vol-0aaaaaaaaaaaaaaa2").Reason)
	assert.Equal(t, KindTag, itemFor(res, "i-0aaaaaaaaaaaaaaaf").Kind, "tags of a resource with no record are residue")
	assert.Empty(t, r.deleter.deleted, "a re-read deletes nothing")
	assert.Empty(t, r.tags.deleted, "a re-read clears no tag")
}

func TestStorageRemainingUnreadStoreIsIncompleteNeverClean(t *testing.T) {
	for name, mutate := range map[string]func(*storageRig){
		"volumes":   func(r *storageRig) { r.volumes.failAt = -1 },
		"instances": func(r *storageRig) { r.instances.out, r.instances.err = nil, errors.New("node silent") },
		"teardown":  func(r *storageRig) { r.teardown.err = errors.New("no responder") },
		"tags":      func(r *storageRig) { r.tags.scanErr = errors.New("tag index unreadable") },
	} {
		t.Run(name, func(t *testing.T) {
			r := newStorageRig()
			r.volumes.vols = nil
			mutate(r)
			res, err := r.cleanup().StorageRemaining(t.Context(), testAccount)
			require.NoError(t, err)
			assert.Equal(t, StatusIncomplete, res.Status)
			assert.NotEmpty(t, res.IncompleteReasons)
		})
	}
}
