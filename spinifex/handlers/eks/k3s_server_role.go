package handlers_eks

import (
	"log/slog"

	handlers_iam "github.com/mulgadc/spinifex/spinifex/handlers/iam"
)

const (
	// eksServerSystemRoleName is the Spinifex-managed instance role attached to
	// the k3s control-plane VM. IMDS serves it so the VM's gateway publishes are
	// signed with scoped, rotating credentials instead of a baked static key.
	eksServerSystemRoleName = "spinifex-eks-server"

	// eksServerInlinePolicyName is the inline policy granting the CP VM the
	// internal gateway actions its bootstrap/state-report/addon-fetch need.
	eksServerInlinePolicyName = "spinifex-eks-server-internal"

	// eksServerInlinePolicy grants only the internal gateway actions the CP VM
	// calls: PublishInternal (bootstrap/state) and ListInternalAddons (addon
	// fetch). The gateway evaluates these per request against the role's policies.
	eksServerInlinePolicy = `{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Action":["eks:PublishInternal","eks:ListInternalAddons"],"Resource":"*"}]}`
)

// ensureCPInstanceProfile returns the CP instance-profile ARN, or "" to signal
// the caller should fall back to static creds. IAM unwired or a transient
// failure both fall back rather than brick a cluster launch — the static-key
// path still authenticates (it is what shipped before IMDS creds).
func (s *EKSServiceImpl) ensureCPInstanceProfile(accountID string) string {
	if s.deps.IAM == nil {
		slog.Warn("EKS: IAM service unwired; CP VM falls back to baked static gateway creds")
		return ""
	}
	profileARN, err := handlers_iam.EnsureSystemInstanceProfile(s.deps.IAM, accountID,
		eksServerSystemRoleName, eksServerInlinePolicyName, eksServerInlinePolicy)
	if err != nil {
		slog.Warn("EKS: ensure CP instance profile failed; falling back to baked static gateway creds",
			"err", err)
		return ""
	}
	return profileARN
}
