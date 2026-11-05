package awsgw

import (
	"context"

	"github.com/aws/aws-sdk-go/service/sts"
	stsdomain "github.com/mulgadc/spinifex/spinifex/domains/sts"
)

// lambdaEnvironmentRoleIssuer is the in-process STS entry point the test-link
// executor uses to issue and revoke an execution environment's role session.
type lambdaEnvironmentRoleIssuer interface {
	AssumeRoleForLambdaEnvironment(ctx context.Context, req stsdomain.LambdaEnvironmentSessionRequest) (*sts.AssumeRoleOutput, error)
	RevokeLambdaEnvironmentSession(ctx context.Context, accessKeyID string, scope stsdomain.LambdaEnvironmentSessionScope) error
}

var _ lambdaEnvironmentRoleIssuer = (*stsdomain.STSServiceImpl)(nil)
