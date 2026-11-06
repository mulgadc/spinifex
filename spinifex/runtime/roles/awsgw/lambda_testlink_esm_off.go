//go:build !lambdatestlink

package awsgw

import (
	"context"
	"net/http"

	"github.com/mulgadc/spinifex/spinifex/domains/lambda/awsapi"
	"github.com/mulgadc/spinifex/spinifex/domains/lambda/eventsource"
)

// Without the test link no mapping is polled and CreateEventSourceMapping has
// no queue reader, so it refuses; Lambda is unrouted there anyway.
type esmRuntime struct{}

func newESMRuntime(func(context.Context) (*eventsource.Store, error), lambdaEnvironmentRoleIssuer, string) *esmRuntime {
	return &esmRuntime{}
}

func (*esmRuntime) sources() awsapi.SourceChecker { return nil }

func (*esmRuntime) start(context.Context, http.Handler, awsapi.Deps, string) {}
