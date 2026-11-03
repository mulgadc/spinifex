//go:build lambdatestlink

package awsgw

import (
	"context"

	"github.com/mulgadc/spinifex/spinifex/domains/lambda/awsapi"
	"github.com/mulgadc/spinifex/spinifex/domains/lambda/codestore"
	"github.com/mulgadc/spinifex/spinifex/domains/lambda/execution"
	"github.com/mulgadc/spinifex/spinifex/ingress/aws/dispatch"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
)

// lambdaTestExecutor sends each Invoke to this node's test-link daemon.
func lambdaTestExecutor(nc *nats.Conn, node string, codes *codestore.Manager) awsapi.Executor {
	return &testLinkExecutor{
		NATS: nc,
		Node: node,
		Codes: func(ctx context.Context) (codePresigner, error) {
			store, err := codes.Store(ctx)
			if err != nil {
				return nil, err
			}
			return store, nil
		},
		Grants: func(ctx context.Context) (grantIssuer, error) {
			js, err := jetstream.New(nc)
			if err != nil {
				return nil, err
			}
			return execution.Open(ctx, js)
		},
	}
}

// lambdaTestRoutes routes Lambda only in test-link builds.
func lambdaTestRoutes(lambda dispatch.Registration) []dispatch.Registration {
	return []dispatch.Registration{lambda}
}
