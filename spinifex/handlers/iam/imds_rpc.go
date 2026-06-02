package handlers_iam

import (
	"github.com/aws/aws-sdk-go/service/iam"
	"github.com/mulgadc/spinifex/spinifex/utils"
	"github.com/nats-io/nats.go"
)

// imdsResponderQueue load-balances the IMDS control-plane RPCs across every
// awsgw in the cluster, matching the cluster-wide worker convention.
const imdsResponderQueue = "spinifex-workers"

// SubjectResolveInstanceProfile and SubjectGetRole are the internal, ACL-gated
// request/reply subjects the IMDS handler (hosted in vpcd) calls to resolve an
// instance profile and its role. IAM runs in-process inside awsgw; only these
// two narrow read RPCs are surfaced. Neither is reachable from a guest.
const (
	SubjectResolveInstanceProfile = "imds.iam.resolve_instance_profile"
	SubjectGetRole                = "imds.iam.get_role"
)

// ResolveInstanceProfileRequest is the SubjectResolveInstanceProfile payload.
type ResolveInstanceProfileRequest struct {
	AccountID string `json:"account_id"`
	NameOrARN string `json:"name_or_arn"`
}

// GetRoleRequest is the SubjectGetRole payload.
type GetRoleRequest struct {
	AccountID string            `json:"account_id"`
	Input     *iam.GetRoleInput `json:"input"`
}

// SubscribeIMDSResponders wires the profile/role responders onto nc,
// queue-grouped so any awsgw can answer. The returned subscriptions are for
// caller-side cleanup.
func (s *IAMServiceImpl) SubscribeIMDSResponders(nc *nats.Conn) ([]*nats.Subscription, error) {
	profileSub, err := nc.QueueSubscribe(SubjectResolveInstanceProfile, imdsResponderQueue, func(msg *nats.Msg) {
		utils.ServeNATSRequest(msg, func(req *ResolveInstanceProfileRequest) (*InstanceProfile, error) {
			return s.ResolveInstanceProfile(req.AccountID, req.NameOrARN)
		})
	})
	if err != nil {
		return nil, err
	}

	roleSub, err := nc.QueueSubscribe(SubjectGetRole, imdsResponderQueue, func(msg *nats.Msg) {
		utils.ServeNATSRequest(msg, func(req *GetRoleRequest) (*iam.GetRoleOutput, error) {
			return s.GetRole(req.AccountID, req.Input)
		})
	})
	if err != nil {
		_ = profileSub.Unsubscribe()
		return nil, err
	}

	return []*nats.Subscription{profileSub, roleSub}, nil
}
