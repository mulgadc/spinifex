package daemon

import (
	"context"

	ec2v1 "github.com/mulgadc/spinifex/contracts/ec2/v1"
)

// sealAccountSettings serves the irreversible EC2 settings seal. The account
// comes from the request body, not X-Account-ID: the sealed account is part
// of the versioned contract, so the header is ignored.
func (d *Daemon) sealAccountSettings(ctx context.Context, req *ec2v1.SealAccountSettingsRequest, _ string) (*ec2v1.SealAccountSettingsReply, error) {
	seal, err := d.accountService.SealAccount(ctx, req.AccountID)
	if err != nil {
		return nil, err
	}
	return &ec2v1.SealAccountSettingsReply{
		AccountID:      req.AccountID,
		CorrelationRef: req.CorrelationRef,
		Disposition:    ec2v1.AccountSettingsDispositionSealed,
		Revision:       seal.Revision,
	}, nil
}
