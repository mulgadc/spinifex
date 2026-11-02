package daemon

import (
	"context"

	ec2v1 "github.com/mulgadc/spinifex/contracts/ec2/v1"
	"github.com/mulgadc/spinifex/spinifex/domains/ec2/accountinventory"
)

// platformInventory serves the read-only EC2 platform inventory. The account
// comes from the request body, not X-Account-ID, so the scanned account is
// part of the versioned contract; the header is ignored.
func (d *Daemon) platformInventory(ctx context.Context, req *ec2v1.PlatformInventoryRequest, _ string) (*ec2v1.PlatformInventoryReply, error) {
	inventory := accountinventory.New(d.keyService, d.placementGroupService, d.launchTemplateService, d.tagsService)
	return inventory.PlatformInventory(ctx, req)
}
