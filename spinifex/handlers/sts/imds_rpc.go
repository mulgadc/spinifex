package handlers_sts

import (
	"github.com/aws/aws-sdk-go/service/sts"
	"github.com/mulgadc/spinifex/spinifex/utils"
	"github.com/nats-io/nats.go"
)

// imdsResponderQueue load-balances the IMDS control-plane RPCs across every
// awsgw in the cluster, matching the cluster-wide worker convention.
const imdsResponderQueue = "spinifex-workers"

// SubjectAssumeRoleForInstance is the internal, ACL-gated request/reply subject
// the IMDS handler (hosted in vpcd) calls to mint instance-role credentials.
// STS keeps the IAM master key in-process inside awsgw; only this narrow RPC is
// surfaced. It is never reachable from a guest.
const SubjectAssumeRoleForInstance = "imds.sts.assume_role_for_instance"

// AssumeRoleForInstanceRequest is the SubjectAssumeRoleForInstance payload. The
// account ID is data here (resolved from the requesting ENI), not an
// authenticated caller identity.
type AssumeRoleForInstanceRequest struct {
	AccountID       string `json:"account_id"`
	RoleARN         string `json:"role_arn"`
	InstanceID      string `json:"instance_id"`
	DurationSeconds int64  `json:"duration_seconds"`
}

// SubscribeIMDSResponder wires the AssumeRoleForInstance responder onto nc,
// queue-grouped so any awsgw can answer.
func (s *STSServiceImpl) SubscribeIMDSResponder(nc *nats.Conn) (*nats.Subscription, error) {
	return nc.QueueSubscribe(SubjectAssumeRoleForInstance, imdsResponderQueue, func(msg *nats.Msg) {
		utils.ServeNATSRequest(msg, func(req *AssumeRoleForInstanceRequest) (*sts.AssumeRoleOutput, error) {
			return s.AssumeRoleForInstance(req.AccountID, req.RoleARN, req.InstanceID, req.DurationSeconds)
		})
	})
}
