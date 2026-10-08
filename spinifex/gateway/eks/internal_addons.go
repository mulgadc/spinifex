package gateway_eks

import (
	"context"
	"errors"

	eksv1 "github.com/mulgadc/spinifex/contracts/eks/v1"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	handlers_eks "github.com/mulgadc/spinifex/spinifex/handlers/eks"
	"github.com/nats-io/nats.go"
)

// ListInternalAddons — GET /clusters/{name}/internal-addons?accountId={acct}.
// Internal control-plane VM route (not an AWS-SDK action): the CP VM holds
// system SigV4 creds, so accountID names the customer cluster account explicitly
// — same carve-out as PublishInternal, gated by AuthorizeInternal, which admits
// only a CP agent serving that account's cluster. Returns every staged manifest so
// the VM can render the baked bundles into the K3s auto-deploy dir and GC the
// locally-rendered manifests for add-ons no longer staged.
func ListInternalAddons(ctx context.Context, natsConn *nats.Conn, clusterName, accountID string) (*eksv1.InternalAddonsResponse, error) {
	if natsConn == nil {
		return nil, errors.New(awserrors.ErrorServerInternal)
	}
	if clusterName == "" || accountID == "" {
		return nil, errors.New(awserrors.ErrorInvalidParameterValue)
	}
	out, err := handlers_eks.NewNATSEKSService(natsConn).ListStagedAddonManifests(ctx,
		&handlers_eks.ListStagedAddonManifestsInput{ClusterName: clusterName}, accountID)
	if err != nil {
		return nil, err
	}
	return &eksv1.InternalAddonsResponse{Addons: out.Manifests}, nil
}
