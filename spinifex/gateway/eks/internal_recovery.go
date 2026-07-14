package gateway_eks

import (
	"context"
	"errors"

	"github.com/mulgadc/spinifex/spinifex/awserrors"
	handlers_eks "github.com/mulgadc/spinifex/spinifex/handlers/eks"
	"github.com/nats-io/nats.go"
)

// internalRecoveryOutput is the body returned to the on-VM k3s-recovery agent:
// the member's current recovery directive.
type internalRecoveryOutput struct {
	Directive handlers_eks.RecoveryDirective `json:"directive"`
}

// GetRecoveryDirective — GET /clusters/{name}/internal-recovery/{acct}/{instanceId}.
// Internal control-plane VM route (not an AWS-SDK action): the CP VM holds system
// SigV4 creds, so accountID names the customer cluster account explicitly — same
// carve-out as ListInternalAddons. Returns the per-member recovery directive the
// agent applies (none, cluster-reset, or wipe-rejoin) before k3s starts.
func GetRecoveryDirective(ctx context.Context, natsConn *nats.Conn, clusterName, accountID, instanceID string) (*internalRecoveryOutput, error) {
	if natsConn == nil {
		return nil, errors.New(awserrors.ErrorServerInternal)
	}
	if clusterName == "" || accountID == "" || instanceID == "" {
		return nil, errors.New(awserrors.ErrorInvalidParameterValue)
	}
	out, err := handlers_eks.NewNATSEKSService(natsConn).GetRecoveryDirective(
		ctx, &handlers_eks.GetRecoveryDirectiveInput{ClusterName: clusterName, InstanceID: instanceID}, accountID)
	if err != nil {
		return nil, err
	}
	return &internalRecoveryOutput{Directive: out.Directive}, nil
}
