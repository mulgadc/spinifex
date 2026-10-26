package daemon

import (
	"context"

	ec2v1 "github.com/mulgadc/spinifex/contracts/ec2/v1"
	awserrors "github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
)

// accountSettingsSealCapability serves this daemon's read-only seal capability.
// It reads no KV and takes no account; a request naming another node is a
// caller mistake, since the subject is already addressed to this node.
func (d *Daemon) accountSettingsSealCapability(_ context.Context, req *ec2v1.AccountSettingsSealCapabilityRequest, _ string) (*ec2v1.AccountSettingsSealCapabilityReply, error) {
	if req.Node != d.node {
		return nil, awserrors.Errorf(awserrors.ErrorInvalidParameterValue,
			"seal capability request for node %q answered by node %q", req.Node, d.node)
	}
	return &ec2v1.AccountSettingsSealCapabilityReply{
		Node:              d.node,
		CapabilityVersion: ec2v1.AccountSettingsSealCapabilityVersion,
		CorrelationRef:    req.CorrelationRef,
	}, nil
}
