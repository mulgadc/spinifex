package handlers_elbv2

import (
	"log/slog"

	handlers_iam "github.com/mulgadc/spinifex/spinifex/handlers/iam"
)

const (
	// lbAgentSystemRoleName is the Spinifex-managed instance role attached to an
	// LB VM. IMDS serves it so the lb-agent signs its gateway polls with scoped,
	// rotating credentials instead of the baked system static key.
	lbAgentSystemRoleName = "spinifex-lb-agent"

	// lbAgentInlinePolicyName is the inline policy granting the lb-agent the two
	// gateway actions it calls.
	lbAgentInlinePolicyName = "spinifex-lb-agent-internal"

	// lbAgentInlinePolicy grants only the actions the lb-agent invokes:
	// LBAgentHeartbeat (target health) and GetLBConfig (poll listener/target
	// config). The gateway evaluates these per request against the role policies.
	lbAgentInlinePolicy = `{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Action":["elasticloadbalancing:LBAgentHeartbeat","elasticloadbalancing:GetLBConfig"],"Resource":"*"}]}`
)

// ensureLBInstanceProfile returns the LB instance-profile ARN, or "" to signal
// the caller should fall back to static creds. accountID must be the account the
// LB VM runs under (the system account) — IMDS resolves the profile under the
// instance's account, so a role created anywhere else is invisible to it. The
// resulting system-account principal satisfies the LBAgentHeartbeat / GetLBConfig
// guard via its GlobalAccountID branch. IAM unwired or a transient failure both
// fall back rather than block an LB launch.
func (s *ELBv2ServiceImpl) ensureLBInstanceProfile(accountID string) string {
	iam := s.IAM
	if iam == nil && s.IAMProvider != nil {
		iam = s.IAMProvider()
	}
	if iam == nil {
		slog.Warn("ELBv2: IAM service unwired; LB VM falls back to baked static gateway creds")
		return ""
	}
	profileARN, err := handlers_iam.EnsureSystemInstanceProfile(iam, accountID,
		lbAgentSystemRoleName, lbAgentInlinePolicyName, lbAgentInlinePolicy)
	if err != nil {
		slog.Warn("ELBv2: ensure LB instance profile failed; falling back to baked static gateway creds",
			"accountId", accountID, "err", err)
		return ""
	}
	return profileARN
}
