package awsgw

import (
	"context"

	"github.com/aws/aws-sdk-go/service/sts"
	"github.com/mulgadc/spinifex/spinifex/domains/lambda/eventsource"
	stsdomain "github.com/mulgadc/spinifex/spinifex/domains/sts"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
)

// lambdaEnvironmentRoleIssuer is the in-process STS entry point the test-link
// executor uses to issue and revoke an execution environment's role session.
type lambdaEnvironmentRoleIssuer interface {
	AssumeRoleForLambdaEnvironment(ctx context.Context, req stsdomain.LambdaEnvironmentSessionRequest) (*sts.AssumeRoleOutput, error)
	RevokeLambdaEnvironmentSession(ctx context.Context, accessKeyID string, scope stsdomain.LambdaEnvironmentSessionScope) error
}

var _ lambdaEnvironmentRoleIssuer = (*stsdomain.STSServiceImpl)(nil)

// lambdaMappingStore opens the event-source mapping bucket on first use.
func lambdaMappingStore(nc *nats.Conn) func(ctx context.Context) (*eventsource.Store, error) {
	return func(ctx context.Context) (*eventsource.Store, error) {
		js, err := jetstream.New(nc)
		if err != nil {
			return nil, err
		}
		return eventsource.Open(ctx, js)
	}
}
