package gateway_eks

import (
	"context"
	"encoding/json"
	"errors"
	eksv1 "github.com/mulgadc/spinifex/contracts/eks/v1"
	"github.com/mulgadc/spinifex/spinifex/foundation/messaging/nats"
	"log/slog"

	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	handlers_eks "github.com/mulgadc/spinifex/spinifex/handlers/eks"
	"github.com/nats-io/nats.go"
)

// VM→host relay channels.
const (
	internalChannelBootstrap = "bootstrap"
	internalChannelState     = "state"
	internalChannelAddon     = "addon"
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

// decodeInternalPublish is the one reading of the body that both AuthorizeInternal
// and PublishInternal use, so the account the gate binds is the account the
// published subject names.
func decodeInternalPublish(body []byte) (internalPublishRequest, error) {
	var req internalPublishRequest
	if err := json.Unmarshal(body, &req); err != nil {
		return internalPublishRequest{}, err
	}
	if req.AccountID == "" {
		return internalPublishRequest{}, errors.New("accountId is required")
	}
	return req, nil
}

// PublishInternal — POST /clusters/{name}/internal-publish. Relays a VM
// publication onto the bootstrap/state/addon NATS subjects via the AWSGW,
// keeping NATS cluster-internal. AuthorizeInternal has bound the caller first.
func PublishInternal(ctx context.Context, natsConn *nats.Conn, clusterName string, body []byte) (*publishInternalOutput, error) {
	if natsConn == nil {
		return nil, errors.New(awserrors.ErrorServerInternal)
	}
	if clusterName == "" {
		return nil, errors.New(awserrors.ErrorInvalidParameterValue)
	}

	req, err := decodeInternalPublish(body)
	if err != nil {
		slog.DebugContext(ctx, "PublishInternal: bad body", "cluster", clusterName, "err", err)
		return nil, errors.New(awserrors.ErrorInvalidParameterValue)
	}
	if len(req.Payload) == 0 {
		return nil, errors.New(awserrors.ErrorInvalidParameterValue)
	}

	var subject string
	switch req.Channel {
	case internalChannelBootstrap:
		if _, ok := validBootstrapKinds[req.Kind]; !ok {
			slog.DebugContext(ctx, "PublishInternal: unknown bootstrap kind", "cluster", clusterName, "kind", req.Kind)
			return nil, errors.New(awserrors.ErrorInvalidParameterValue)
		}
		subject = handlers_eks.BootstrapSubject(req.AccountID, clusterName, req.Kind)
	case internalChannelState:
		subject = handlers_eks.StateSubject(req.AccountID, clusterName)
	case internalChannelAddon:
		subject = eksv1.AddonStatusSubject(req.AccountID, clusterName)
	default:
		slog.DebugContext(ctx, "PublishInternal: unknown channel", "cluster", clusterName, "channel", req.Channel)
		return nil, errors.New(awserrors.ErrorInvalidParameterValue)
	}

	msg := nats.NewMsg(subject)
	msg.Data = req.Payload
	natsmsg.InjectTraceContext(ctx, msg.Header)
	if err := natsConn.PublishMsg(msg); err != nil {
		slog.ErrorContext(ctx, "PublishInternal: NATS publish failed", "subject", subject, "err", err)
		return nil, errors.New(awserrors.ErrorServerInternal)
	}
	slog.DebugContext(ctx, "PublishInternal: relayed", "subject", subject, "bytes", len(req.Payload))

	return &publishInternalOutput{}, nil
}
