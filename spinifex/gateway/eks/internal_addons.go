package gateway_eks

import (
	"context"
	"errors"

	"github.com/mulgadc/spinifex/spinifex/awserrors"
	handlers_eks "github.com/mulgadc/spinifex/spinifex/handlers/eks"
	"github.com/nats-io/nats.go"
)

// internalAddonsOutput is the body returned to the on-VM addon-sync agent: the
// set of add-on manifests currently staged for the cluster.
type internalAddonsOutput struct {
	Addons []handlers_eks.StagedAddonManifest `json:"addons"`
}

// ListInternalAddons — GET /clusters/{name}/internal-addons?accountId={acct}.
// Internal control-plane VM route (not an AWS-SDK action): the CP VM holds
// system SigV4 creds, so accountID names the customer cluster account explicitly
// — same carve-out as PublishInternal. Returns every staged add-on manifest so
// the VM can render the baked bundles into the K3s auto-deploy dir and GC the
// locally-rendered manifests for add-ons no longer staged.
func ListInternalAddons(ctx context.Context, natsConn *nats.Conn, clusterName, accountID string) (*internalAddonsOutput, error) {
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
	return &internalAddonsOutput{Addons: out.Manifests}, nil
}
