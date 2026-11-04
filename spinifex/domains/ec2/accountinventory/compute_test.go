package accountinventory

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	ec2metadata "github.com/mulgadc/spinifex/spinifex/domains/ec2/ebs/metadata"
	ec2spot "github.com/mulgadc/spinifex/spinifex/domains/ec2/spotinstance"
)

type fakeSpot struct {
	scan *ec2spot.AccountScan
	err  error
}

func (f fakeSpot) ScanAccount(context.Context, string) (*ec2spot.AccountScan, error) {
	return f.scan, f.err
}

type fakeVolumes struct {
	docs []ec2metadata.Volume
	err  error
}

func (f fakeVolumes) ListVolumesStrict(context.Context, string) ([]ec2metadata.Volume, error) {
	return f.docs, f.err
}

func TestSpotRequestInventoryMarksActiveAndTerminal(t *testing.T) {
	inv := New(nil, nil, nil, nil).WithCompute(fakeSpot{scan: &ec2spot.AccountScan{
		Active:   []ec2spot.AccountSpotRequest{{RequestID: "sir-1", State: "active"}},
		Terminal: []ec2spot.AccountSpotRequest{{RequestID: "sir-2", State: "cancelled"}},
	}}, nil)

	items, err := inv.SpotRequestInventory(t.Context(), testAccount)
	require.NoError(t, err)
	assert.Equal(t, []SpotRequestItem{
		{RequestID: "sir-1", State: "active", Active: true},
		{RequestID: "sir-2", State: "cancelled"},
	}, items)
}

func TestSpotRequestInventoryScanFailureIsAnErrorNotAShorterList(t *testing.T) {
	inv := New(nil, nil, nil, nil).WithCompute(fakeSpot{err: errList}, nil)

	items, err := inv.SpotRequestInventory(t.Context(), testAccount)
	require.ErrorIs(t, err, errList)
	assert.Nil(t, items)
}

func TestVolumeInventoryReportsAttachmentAndDeletingDocuments(t *testing.T) {
	inv := New(nil, nil, nil, nil).WithCompute(nil, fakeVolumes{docs: []ec2metadata.Volume{
		{VolumeID: "vol-1", State: "in-use", AttachedInstance: "i-1", DeleteOnTermination: true},
		{VolumeID: "vol-2", State: "deleting"},
	}})

	items, err := inv.VolumeInventory(t.Context(), testAccount)
	require.NoError(t, err)
	assert.Equal(t, []VolumeItem{
		{VolumeID: "vol-1", State: "in-use", AttachedInstance: "i-1", DeleteOnTermination: true},
		{VolumeID: "vol-2", State: "deleting"},
	}, items)
}

func TestVolumeInventoryListFailureIsAnErrorNotAShorterList(t *testing.T) {
	inv := New(nil, nil, nil, nil).WithCompute(nil, fakeVolumes{err: errList})

	items, err := inv.VolumeInventory(t.Context(), testAccount)
	require.ErrorIs(t, err, errList)
	assert.Nil(t, items)
}

func TestComputeInventoriesRefuseWhenNotConfigured(t *testing.T) {
	inv := New(nil, nil, nil, nil)
	_, err := inv.SpotRequestInventory(t.Context(), testAccount)
	require.Error(t, err)
	_, err = inv.VolumeInventory(t.Context(), testAccount)
	require.Error(t, err)
}
