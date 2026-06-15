package external

import (
	"context"
	"errors"
	"fmt"

	"github.com/mulgadc/spinifex/spinifex/network/policy"
)

// EIPManager attaches/detaches EIP NAT rules. Thin L5 wrapper over policy.NATManager
// that adds the flows-barrier so callers see the EIP reachable on return.
type EIPManager interface {
	AttachEIP(ctx context.Context, eip policy.EIPSpec) error
	DetachEIP(ctx context.Context, vpcID, externalIP, logicalIP, portName string) error
}

type eipManager struct {
	nat     policy.NATManager
	barrier FlowsBarrier
}

var _ EIPManager = (*eipManager)(nil)

// NewEIPManager constructs an EIPManager. barrier may be nil (no wait).
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

func (m *eipManager) DetachEIP(ctx context.Context, vpcID, externalIP, logicalIP, portName string) error {
	return m.nat.DeleteEIP(ctx, vpcID, externalIP, logicalIP, portName)
}
