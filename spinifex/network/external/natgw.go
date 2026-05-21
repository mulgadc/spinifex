package external

import (
	"context"
	"errors"
	"fmt"

	"github.com/mulgadc/spinifex/spinifex/network/policy"
)

// NATGWManager attaches and detaches NAT Gateway SNAT rules. Thin L5
// wrapper over policy.NATManager — distinct from EIPManager because
// AWS-parity NAT Gateways are owned by a different ec2 resource (NAT GW
// not EIP) and the underlying OVN type differs (snat vs dnat_and_snat).
type NATGWManager interface {
	AttachNATGateway(ctx context.Context, gw policy.NATGWSpec) error
	DetachNATGateway(ctx context.Context, vpcID, subnetCIDR string) error
}

type natGWManager struct {
	nat policy.NATManager
}

var _ NATGWManager = (*natGWManager)(nil)

// NewNATGWManager constructs a NATGWManager.
func NewNATGWManager(nat policy.NATManager) (NATGWManager, error) {
	if nat == nil {
		return nil, errors.New("NATGWManager: NATManager required")
	}
	return &natGWManager{nat: nat}, nil
}

func (m *natGWManager) AttachNATGateway(ctx context.Context, gw policy.NATGWSpec) error {
	if gw.VPCID == "" || gw.PublicIP == "" || gw.SubnetCIDR == "" {
		return fmt.Errorf("AttachNATGateway: VPCID, PublicIP, SubnetCIDR all required")
	}
	return m.nat.AddNATGateway(ctx, gw)
}

func (m *natGWManager) DetachNATGateway(ctx context.Context, vpcID, subnetCIDR string) error {
	return m.nat.DeleteNATGateway(ctx, vpcID, subnetCIDR)
}
