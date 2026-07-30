package handlers_rds

import (
	"encoding/json"
	"log/slog"

	handlers_iam "github.com/mulgadc/spinifex/spinifex/handlers/iam"
)

const (
	// The instance role the in-guest rds-agent assumes through IMDS. The
	// gateway's internal-action gate admits a caller only when the session was
	// assumed from this role, so the name is part of the protocol.
	InstanceRoleName = "rdsInstanceRole"

	instanceRoleInlinePolicyName = "spinifex-rds-instance-internal"
)

// The agent-only actions. The gateway's principal-class gate reserves exactly
// this set, and the role below grants exactly it — one list, so adding a fifth
// cannot leave the gate and the grant disagreeing.
var InternalAgentActions = []string{
	"RegisterDBInstance",
	"SubmitDBStateChange",
	"PollDBCommands",
	"GetDBBootstrapConfig",
}

// Never rds:*: granting the customer surface here would let a Postgres RCE on
// one DB VM manage its account's whole fleet.
var instanceRoleInlinePolicy = func() string {
	actions := make(handlers_iam.StringOrArr, 0, len(InternalAgentActions))
	for _, action := range InternalAgentActions {
		actions = append(actions, "rds:"+action)
	}
	doc, err := json.Marshal(handlers_iam.PolicyDocument{
		Version: "2012-10-17",
		Statement: []handlers_iam.Statement{{
			Effect:   handlers_iam.PolicyEffectAllow,
			Action:   actions,
			Resource: handlers_iam.StringOrArr{"*"},
		}},
	})
	if err != nil {
		panic("rds: cannot marshal the instance-role policy: " + err.Error())
	}
	return string(doc)
}()

// Resolved per launch rather than held, mirroring EKS: the KV-backed IAM
// service has no responders until JetStream is up, so an eager build races
// daemon boot and fails permanently on the node that loses.
type IAMProvider func() handlers_iam.SystemInstanceRoleEnsurer

// Returns the instance-profile ARN, or "" when IAM is unwired or the ensure
// failed. Unlike EKS there is no static-credential fallback: without the role
// the agent cannot authenticate, so an empty ARN launches a VM whose agent
// never registers and the reconciler marks failed.
func ensureInstanceProfile(provider IAMProvider, accountID string) string {
	if provider == nil {
		slog.Warn("rds: IAM unwired; the DB VM agent will have no gateway credentials")
		return ""
	}
	iamSvc := provider()
	if iamSvc == nil {
		slog.Warn("rds: IAM service unavailable; the DB VM agent will have no gateway credentials")
		return ""
	}
	profileARN, err := handlers_iam.EnsureSystemInstanceProfile(iamSvc, accountID,
		InstanceRoleName, instanceRoleInlinePolicyName, instanceRoleInlinePolicy)
	if err != nil {
		slog.Warn("rds: ensure instance profile failed; the DB VM agent will have no gateway credentials",
			"role", InstanceRoleName, "err", err)
		return ""
	}
	return profileARN
}
