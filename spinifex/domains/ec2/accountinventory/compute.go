package accountinventory

import (
	"context"
	"fmt"

	ec2metadata "github.com/mulgadc/spinifex/spinifex/domains/ec2/ebs/metadata"
	ec2spot "github.com/mulgadc/spinifex/spinifex/domains/ec2/spotinstance"
)

// SpotRequestScanner reads one account's spot requests, failing on any record
// it cannot read.
type SpotRequestScanner interface {
	ScanAccount(ctx context.Context, accountID string) (*ec2spot.AccountScan, error)
}

// VolumeScanner reads one account's volume documents, failing on any document
// it cannot read.
type VolumeScanner interface {
	ListVolumesStrict(ctx context.Context, accountID string) ([]ec2metadata.Volume, error)
}

var (
	_ SpotRequestScanner = (*ec2spot.SpotInstanceServiceImpl)(nil)
	_ VolumeScanner      = (*ec2metadata.Store)(nil)
)

// SpotRequestItem is one request the account holds.
type SpotRequestItem struct {
	RequestID string
	State     string
	Active    bool
}

// VolumeItem is one volume document the account holds.
type VolumeItem struct {
	VolumeID            string
	State               string
	AttachedInstance    string
	DeleteOnTermination bool
}

// WithCompute adds the spot-request and volume owner stores.
func (i *Inventory) WithCompute(spot SpotRequestScanner, volumes VolumeScanner) *Inventory {
	i.spot = spot
	i.volumes = volumes
	return i
}

// SpotRequestInventory returns every spot request the account holds. A scan
// that cannot be completed is an error, never a shorter list.
func (i *Inventory) SpotRequestInventory(ctx context.Context, accountID string) ([]SpotRequestItem, error) {
	if i.spot == nil {
		return nil, fmt.Errorf("spot request inventory is not configured")
	}
	scan, err := i.spot.ScanAccount(ctx, accountID)
	if err != nil {
		return nil, fmt.Errorf("spot request inventory incomplete: %w", err)
	}
	items := make([]SpotRequestItem, 0, len(scan.Active)+len(scan.Terminal))
	for _, r := range scan.Active {
		items = append(items, SpotRequestItem{RequestID: r.RequestID, State: r.State, Active: true})
	}
	for _, r := range scan.Terminal {
		items = append(items, SpotRequestItem{RequestID: r.RequestID, State: r.State})
	}
	return items, nil
}

// VolumeInventory returns every volume document the account holds, including
// those already marked deleting. An unreadable document is an error.
func (i *Inventory) VolumeInventory(ctx context.Context, accountID string) ([]VolumeItem, error) {
	if i.volumes == nil {
		return nil, fmt.Errorf("volume inventory is not configured")
	}
	docs, err := i.volumes.ListVolumesStrict(ctx, accountID)
	if err != nil {
		return nil, fmt.Errorf("volume inventory incomplete: %w", err)
	}
	items := make([]VolumeItem, 0, len(docs))
	for _, v := range docs {
		items = append(items, VolumeItem{
			VolumeID: v.VolumeID, State: v.State, AttachedInstance: v.AttachedInstance,
			DeleteOnTermination: v.DeleteOnTermination,
		})
	}
	return items, nil
}
