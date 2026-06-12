package handlers_iam

import (
	"github.com/aws/aws-sdk-go/service/iam"
	"github.com/mulgadc/spinifex/spinifex/utils"
	"github.com/nats-io/nats.go"
)

// imdsResponderQueue load-balances the IMDS control-plane RPCs across every
// awsgw in the cluster, matching the cluster-wide worker convention.
const imdsResponderQueue = "spinifex-workers"

// SubjectResolveInstanceProfile and SubjectGetRole are the ACL-gated internal RPC subjects
// vpcd uses to resolve an instance profile and role from awsgw's IAM service.
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

// SubscribeIMDSResponders wires profile/role responders onto nc, queue-grouped.
// Returns subscriptions for caller-side cleanup.
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
