package gateway_eks

import (
	"encoding/json"
	"errors"
	"log/slog"

	"github.com/mulgadc/spinifex/spinifex/awserrors"
	handlers_eks "github.com/mulgadc/spinifex/spinifex/handlers/eks"
	"github.com/nats-io/nats.go"
)

// internalPublishChannels are the two VM→host channels the broker relays.
const (
	internalChannelBootstrap = "bootstrap"
	internalChannelState     = "state"
)

// internalPublishRequest is the wire shape the on-VM eks-gateway-publish helper
// POSTs to /clusters/{name}/internal-publish. The control-plane VM holds system
// SigV4 creds (system account, not the cluster account), so AccountID names the
// customer cluster account explicitly rather than being taken from the SigV4
// auth context. Payload is the already-encoded subject body (a BootstrapEnvelope
// for the bootstrap channel, a ServerStateReport for the state channel) — the
// broker relays it verbatim onto the existing NATS subject, so the daemon-side
// subscriber, KV persistence, and reconciler are unchanged.
type internalPublishRequest struct {
	AccountID string          `json:"accountId"`
	Channel   string          `json:"channel"`
	Kind      string          `json:"kind"`
	Payload   json.RawMessage `json:"payload"`
}

// publishInternalOutput is the empty success body (`{}`).
type publishInternalOutput struct{}

// validBootstrapKinds is the closed set of bootstrap subject suffixes the broker
// will relay — a malformed/hostile kind cannot be turned into an arbitrary
// subject suffix.
var validBootstrapKinds = map[string]struct{}{
	handlers_eks.BootstrapSubjectToken:      {},
	handlers_eks.BootstrapSubjectKubeconfig: {},
	handlers_eks.BootstrapSubjectJWKS:       {},
	handlers_eks.BootstrapSubjectCA:         {},
}

// PublishInternal — POST /clusters/{name}/internal-publish. Relays a
// control-plane VM publication onto the existing bootstrap/state NATS subjects.
// This is the gateway-broker replacement for the VM dialing core NATS directly:
// the VM reaches the daemon over the mgmt-reachable AWSGW with SigV4 + retry
// (DDIL-correct), and NATS stays cluster-internal.
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
