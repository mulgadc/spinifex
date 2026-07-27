package awsgw

import (
	"encoding/json"
	"log/slog"

	"github.com/mulgadc/spinifex/spinifex/awserrors"
	handlers_eks "github.com/mulgadc/spinifex/spinifex/handlers/eks"
	handlers_sts "github.com/mulgadc/spinifex/spinifex/handlers/sts"
	"github.com/mulgadc/spinifex/spinifex/utils"
	"github.com/nats-io/nats.go"
)

// eksTokenVerifyQueue is the NATS queue group for the EKS token-verify
// responders so the request is load-balanced across awsgw nodes.
const eksTokenVerifyQueue = "awsgw-eks-token-verify" //nolint:gosec // queue-group name, not a credential

// presignedVerifier is the slice of the STS service the token-verify responder
// needs: resolve an `aws eks get-token` presigned URL into a caller principal,
// binding the signature to the expected cluster name (anti-replay, Q10).
type presignedVerifier interface {
	VerifyPresignedGetCallerIdentity(presignedURL, expectedClusterName string) (*handlers_sts.PresignedCallerIdentity, error)
}

var _ presignedVerifier = (*handlers_sts.STSServiceImpl)(nil)

// registerEKSTokenVerify subscribes the awsgw service to TokenVerifySubject so
// the in-cluster eks-token-webhook can resolve get-token presigned URLs over
// NATS (STS is hosted in-process here and is not otherwise on the bus).
func registerEKSTokenVerify(nc *nats.Conn, verifier presignedVerifier) (*nats.Subscription, error) {
	return nc.QueueSubscribe(handlers_eks.TokenVerifySubject, eksTokenVerifyQueue, handleEKSTokenVerify(verifier))
}

func handleEKSTokenVerify(verifier presignedVerifier) nats.MsgHandler {
	return func(msg *nats.Msg) {
		var req handlers_eks.TokenVerifyRequest
		if err := json.Unmarshal(msg.Data, &req); err != nil {
			respondTokenVerifyErr(msg, awserrors.ErrorInvalidParameterValue)
			return
		}
		if req.PresignedURL == "" || req.ClusterName == "" {
			respondTokenVerifyErr(msg, awserrors.ErrorInvalidParameterValue)
			return
		}

		ident, err := verifier.VerifyPresignedGetCallerIdentity(req.PresignedURL, req.ClusterName)
		if err != nil {
			// A verify failure is the common, expected case for a forged/replayed
			// token — log at debug so a port-scan does not flood the error log.
			slog.Debug("EKS token verify rejected", "cluster", req.ClusterName, "err", err)
			respondTokenVerifyErr(msg, awserrors.ValidErrorCodeFromError(err))
			return
		}

		resp := handlers_eks.TokenVerifyResponse{
			AccountID:     ident.AccountID,
			ARN:           ident.ARN,
			UserID:        ident.UserID,
			PrincipalType: ident.PrincipalType,
		}
		data, err := json.Marshal(resp)
		if err != nil {
			respondTokenVerifyErr(msg, awserrors.ErrorServerInternal)
			return
		}
		if err := msg.Respond(data); err != nil {
			slog.Error("EKS token verify: failed to respond", "err", err)
		}
	}
}

func respondTokenVerifyErr(msg *nats.Msg, code string) {
	if err := msg.Respond(utils.GenerateErrorPayload(code)); err != nil {
		slog.Error("EKS token verify: failed to respond with error", "err", err)
	}
}
