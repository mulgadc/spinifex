//go:build lambdatestlink

package awsgw

import (
	"context"
	"log/slog"
	"time"

	"github.com/mulgadc/spinifex/spinifex/domains/lambda/awsapi"
	"github.com/mulgadc/spinifex/spinifex/domains/lambda/codestore"
	"github.com/mulgadc/spinifex/spinifex/domains/lambda/execution"
	"github.com/mulgadc/spinifex/spinifex/ingress/aws/dispatch"
	"github.com/mulgadc/spinifex/spinifex/runtime/compute/lambda/testlink"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
)

// lambdaTestExecutor sends each Invoke to this node's test-link daemon over
// the control socket. Without the daemon's service account it does not serve.
func lambdaTestExecutor(nc *nats.Conn, node string, codes *codestore.Manager, sessions lambdaEnvironmentRoleIssuer) awsapi.Executor {
	daemonUID, err := testlink.LookupUID(testlink.DaemonUser)
	if err != nil {
		slog.Error("Lambda test link: control peer account unavailable; Invoke is not served", "err", err)
		return nil
	}
	return &testLinkExecutor{
		Control: &testlink.ControlClient{
			Name:         testlink.ControlName,
			DaemonUID:    daemonUID,
			RequestLimit: nc.MaxPayload(),
			ReplyLimit:   nc.MaxPayload(),
			IOTimeout:    controlWriteTimeout,
		},
		RequestLimit: nc.MaxPayload(),
		Sessions:     sessions,
		Node:         node,
		ImageDigest:  lambdaImageDigest,
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

// lambdaImageDigest pins the runtime image this gateway expects the daemon
// to run; make build-lambda-testlink sets it to the daemon's pin.
var lambdaImageDigest string

// controlWriteTimeout bounds writing one request to the local daemon.
const controlWriteTimeout = 30 * time.Second

// lambdaTestRoutes routes Lambda only in test-link builds.
func lambdaTestRoutes(lambda dispatch.Registration) []dispatch.Registration {
	return []dispatch.Registration{lambda}
}
