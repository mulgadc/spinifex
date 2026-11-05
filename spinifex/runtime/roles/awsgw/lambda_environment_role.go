package awsgw

import (
	"context"

	"github.com/aws/aws-sdk-go/service/sts"
	stsdomain "github.com/mulgadc/spinifex/spinifex/domains/sts"
)

// lambdaEnvironmentRoleIssuer is the in-process STS entry point a future Lambda
// executor uses for an execution environment's role session. Nothing calls it yet.
type lambdaEnvironmentRoleIssuer interface {
	AssumeRoleForLambdaEnvironment(ctx context.Context, req stsdomain.LambdaEnvironmentSessionRequest) (*sts.AssumeRoleOutput, error)
}

var _ lambdaEnvironmentRoleIssuer = (*stsdomain.STSServiceImpl)(nil)
