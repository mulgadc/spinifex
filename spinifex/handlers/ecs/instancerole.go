package handlers_ecs

import (
	"fmt"

	handlers_iam "github.com/mulgadc/spinifex/spinifex/handlers/iam"
)

const (
	// ecsInstanceRoleName is the role + instance-profile name AWS uses for ECS
	// container instances. The agent draws its creds from this role over IMDS.
	ecsInstanceRoleName = "ecsInstanceRole"

	// ecsInstanceRolePolicyName is the inline policy granting the agent the
	// actions the gateway enforces for its assumed-role principal.
	ecsInstanceRolePolicyName = "ecsInstanceRolePolicy"
)

// ecsInstanceRolePolicyDoc grants the agent the ECS control-plane actions the
// gateway enforces for assumed-role principals, ECR read so it can pull task
// images when a task carries no execution role, and sts:AssumeRole so it can
// mint task and execution role credentials. Assumption stays bounded by each
// target role's trust policy.
func ecsInstanceRolePolicyDoc(accountID string) string {
	return fmt.Sprintf(`{"Version":"2012-10-17","Statement":[`+
		`{"Effect":"Allow","Action":"ecs:*","Resource":"*"},`+
		`{"Effect":"Allow","Action":["ecr:GetAuthorizationToken","ecr:BatchCheckLayerAvailability",`+
		`"ecr:GetDownloadUrlForLayer","ecr:BatchGetImage"],"Resource":"*"},`+
		`{"Effect":"Allow","Action":"sts:AssumeRole","Resource":"arn:aws:iam::%s:role/*"}]}`, accountID)
}

// ensureECSInstanceProfile find-or-creates the ecsInstanceRole, re-asserts its
// inline policy, and binds the matching instance profile, returning the profile
// ARN. Every step is idempotent so concurrent callers converge, and the policy
// is written rather than created so an existing role picks up a changed
// document instead of keeping the one it was provisioned with.
func (s *Service) ensureECSInstanceProfile(accountID string) (string, error) {
	return handlers_iam.EnsureSystemInstanceProfile(s.deps.IAM, accountID,
		ecsInstanceRoleName, ecsInstanceRolePolicyName, ecsInstanceRolePolicyDoc(accountID))
}

// convergeECSInstanceRole re-asserts the inline policy on an account that
// already has the role, so a running cluster picks up a changed document
// without creating IAM entities in accounts that never launched capacity.
func (s *Service) convergeECSInstanceRole(accountID string) error {
	return handlers_iam.ConvergeSystemRolePolicy(s.deps.IAM, accountID,
		ecsInstanceRoleName, ecsInstanceRolePolicyName, ecsInstanceRolePolicyDoc(accountID))
}
