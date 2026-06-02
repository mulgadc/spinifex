package daemon

import (
	"errors"

	handlers_eks "github.com/mulgadc/spinifex/spinifex/handlers/eks"
)

// daemonEKSSubnetResolver adapts the daemon's VPCServiceImpl to the
// handlers_eks subnetVPCResolver-equivalent. handlers/eks declares the
// interface privately; the daemon needs an exported shape only because the
// constructor wires it via EKSServiceDeps.SubnetResolver = ...
//
// The translation is trivial: VPCService.GetSubnet returns a SubnetRecord
// with VpcId, which is the only field the EKS launcher needs.
type daemonEKSSubnetResolver struct {
	d *Daemon
}

func (a *daemonEKSSubnetResolver) GetSubnetVPC(accountID, subnetID string) (string, error) {
	if a.d == nil || a.d.vpcService == nil {
		return "", errors.New("eks: VPC service not initialized")
	}
	rec, err := a.d.vpcService.GetSubnet(accountID, subnetID)
	if err != nil {
		return "", err
	}
	return rec.VpcId, nil
}

var _ handlers_eks.SubnetVPCResolver = (*daemonEKSSubnetResolver)(nil)
