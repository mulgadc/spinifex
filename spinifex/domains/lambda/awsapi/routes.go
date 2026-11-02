package awsapi

import (
	"context"
	"net/url"
	"slices"

	"github.com/aws/aws-sdk-go/service/lambda"
	"github.com/mulgadc/spinifex/spinifex/ingress/aws/rest"
)

// call is one authorized request as a handler sees it.
type call struct {
	AccountID string
	Region    string
	Params    []string
	Query     url.Values
	Body      []byte
	// Create is the decoded CreateFunction body, decoded once ahead of the
	// authorization gate and the PassRole check that both read it.
	Create *lambda.CreateFunctionInput
}

// reply is a handler's success: the HTTP status and the SDK shape to encode.
type reply struct {
	Status int
	Body   any
}

type route = rest.Route[routeHandler]

type routeHandler func(ctx context.Context, d Deps, c *call) (reply, error)

// routes is the dispatch table. Lambda API version 2015-03-31.
var routes = []route{
	{Method: "POST", Pattern: "/2015-03-31/functions", Action: "CreateFunction",
		Handler: func(ctx context.Context, d Deps, c *call) (reply, error) {
			return d.createFunction(ctx, c)
		}},
	{Method: "GET", Pattern: "/2015-03-31/functions/{FunctionName}", Action: "GetFunction",
		Handler: func(ctx context.Context, d Deps, c *call) (reply, error) {
			return d.getFunction(ctx, c)
		}},
}

// actionNames returns the distinct actions in routes in stable order.
func actionNames() []string {
	names := make([]string, 0, len(routes))
	for _, r := range routes {
		names = append(names, r.Action)
	}
	slices.Sort(names)
	return slices.Compact(names)
}

var router = rest.NewRouter("lambda", routes)
