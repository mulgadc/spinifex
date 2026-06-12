package gateway_eks

import (
	"encoding/json"
	"errors"
	"log/slog"

	"github.com/mulgadc/spinifex/spinifex/awserrors"
	handlers_eks "github.com/mulgadc/spinifex/spinifex/handlers/eks"
	"github.com/nats-io/nats.go"
)

// VM→host relay channels.
const (
	internalChannelBootstrap = "bootstrap"
	internalChannelState     = "state"
)

// internalPublishRequest is the body POSTed to /clusters/{name}/internal-publish.
// The VM uses system SigV4 creds, so AccountID names the cluster account
// explicitly. Payload is the pre-encoded subject body relayed verbatim onto NATS.
type internalPublishRequest struct {
	AccountID string          `json:"accountId"`
	Channel   string          `json:"channel"`
	Kind      string          `json:"kind"`
	Payload   json.RawMessage `json:"payload"`
}

// publishInternalOutput is the empty success body (`{}`).
type publishInternalOutput struct{}

// validBootstrapKinds is the closed set of bootstrap subject suffixes the
// broker will relay, preventing arbitrary subject injection.
var validBootstrapKinds = map[string]struct{}{
	handlers_eks.BootstrapSubjectToken:      {},
	handlers_eks.BootstrapSubjectKubeconfig: {},
	handlers_eks.BootstrapSubjectJWKS:       {},
	handlers_eks.BootstrapSubjectCA:         {},
}

// PublishInternal — POST /clusters/{name}/internal-publish. Relays a VM
// publication onto the bootstrap/state NATS subjects via the AWSGW, keeping
// NATS cluster-internal.
func PublishInternal(natsConn *nats.Conn, clusterName string, body []byte) (*publishInternalOutput, error) {
	if natsConn == nil {
		return nil, errors.New(awserrors.ErrorServerInternal)
	}
	if clusterName == "" {
		return nil, errors.New(awserrors.ErrorInvalidParameterValue)
	}

	var req internalPublishRequest
	if err := json.Unmarshal(body, &req); err != nil {
		slog.Debug("PublishInternal: bad body", "cluster", clusterName, "err", err)
		return nil, errors.New(awserrors.ErrorInvalidParameterValue)
	}
	if req.AccountID == "" || len(req.Payload) == 0 {
		return nil, errors.New(awserrors.ErrorInvalidParameterValue)
	}

	var subject string
	switch req.Channel {
	case internalChannelBootstrap:
		if _, ok := validBootstrapKinds[req.Kind]; !ok {
			slog.Debug("PublishInternal: unknown bootstrap kind", "cluster", clusterName, "kind", req.Kind)
			return nil, errors.New(awserrors.ErrorInvalidParameterValue)
		}
		subject = handlers_eks.BootstrapSubject(req.AccountID, clusterName, req.Kind)
	case internalChannelState:
		subject = handlers_eks.StateSubject(req.AccountID, clusterName)
	default:
		slog.Debug("PublishInternal: unknown channel", "cluster", clusterName, "channel", req.Channel)
		return nil, errors.New(awserrors.ErrorInvalidParameterValue)
	}

	if err := natsConn.Publish(subject, req.Payload); err != nil {
		slog.Error("PublishInternal: NATS publish failed", "subject", subject, "err", err)
		return nil, errors.New(awserrors.ErrorServerInternal)
	}
	slog.Debug("PublishInternal: relayed", "subject", subject, "bytes", len(req.Payload))

	return &publishInternalOutput{}, nil
}
