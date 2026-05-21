package external

import (
	"context"
	"errors"
	"fmt"

	"github.com/mulgadc/spinifex/spinifex/network/policy"
)

// EIPManager attaches and detaches Elastic IP NAT rules. It is a thin L5
// wrapper over policy.NATManager that adds the flows-barrier — without it,
// a freshly attached EIP is unreachable on its public IP until northd
// compiles + every chassis installs the new flows (mulga-siv-105).
//
// Pool allocation lives upstream in handlers/ec2/eip; this manager only
// turns an already-allocated public IP into an OVN dnat_and_snat rule.
type EIPManager interface {
	AttachEIP(ctx context.Context, eip policy.EIPSpec) error
	DetachEIP(ctx context.Context, vpcID, logicalIP string) error
}

type eipManager struct {
	nat     policy.NATManager
	barrier FlowsBarrier
}

var _ EIPManager = (*eipManager)(nil)

// NewEIPManager constructs an EIPManager. barrier may be nil (tests skip
// the wait); production wiring injects `ovn-nbctl --wait=hv sync`.
func NewEIPManager(nat policy.NATManager, barrier FlowsBarrier) (EIPManager, error) {
	if nat == nil {
		return nil, errors.New("EIPManager: NATManager required")
	}
	if barrier == nil {
		barrier = func() error { return nil }
	}
	return &eipManager{nat: nat, barrier: barrier}, nil
}

func (m *eipManager) AttachEIP(ctx context.Context, eip policy.EIPSpec) error {
	if eip.VPCID == "" || eip.ExternalIP == "" || eip.LogicalIP == "" {
		return fmt.Errorf("AttachEIP: VPCID, ExternalIP, LogicalIP all required")
	}
	if err := m.nat.AddEIP(ctx, eip); err != nil {
		return err
	}
	_ = m.barrier()
	return nil
}

func (m *eipManager) DetachEIP(ctx context.Context, vpcID, logicalIP string) error {
	return m.nat.DeleteEIP(ctx, vpcID, logicalIP)
}
