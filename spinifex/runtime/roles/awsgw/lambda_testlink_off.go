//go:build !lambdatestlink

package awsgw

import (
	"github.com/mulgadc/spinifex/spinifex/domains/lambda/awsapi"
	"github.com/mulgadc/spinifex/spinifex/domains/lambda/codestore"
	"github.com/mulgadc/spinifex/spinifex/ingress/aws/dispatch"
	"github.com/nats-io/nats.go"
)

// Without the test link there is no executor, so Invoke refuses before it
// presigns, sends or allocates anything, and Lambda stays unrouted.
func lambdaTestExecutor(*nats.Conn, string, *codestore.Manager, lambdaEnvironmentRoleIssuer) awsapi.Executor {
	return nil
}

func lambdaTestRoutes(dispatch.Registration) []dispatch.Registration { return nil }
