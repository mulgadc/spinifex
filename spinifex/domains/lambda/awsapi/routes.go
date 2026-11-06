package awsapi

import (
	"context"
	"net/http"
	"net/url"

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
	Header    http.Header
	// Create is the decoded CreateFunction body, decoded once ahead of the
	// authorization gate and the PassRole check that both read it.
	Create *lambda.CreateFunctionInput
	// Invoke is Invoke's validated target, resolved ahead of the
	// authorization gate whose resource it names.
	Invoke *invokeTarget
}

// rawJSON is a response body written as is, for a shape the SDK encoder
// cannot produce.
type rawJSON []byte

// reply is a handler's success: the HTTP status and the SDK shape to encode.
type reply struct {
	Status int
	Body   any
	Header http.Header
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
	{Method: "GET", Pattern: "/2015-03-31/functions/{FunctionName}/configuration", Action: "GetFunctionConfiguration",
		Handler: func(ctx context.Context, d Deps, c *call) (reply, error) {
			return d.getFunctionConfiguration(ctx, c)
		}},
	{Method: "GET", Pattern: "/2015-03-31/functions/{FunctionName}/versions", Action: "ListVersionsByFunction",
		Handler: func(ctx context.Context, d Deps, c *call) (reply, error) {
			return d.listVersionsByFunction(ctx, c)
		}},
	{Method: "GET", Pattern: "/2020-06-30/functions/{FunctionName}/code-signing-config", Action: "GetFunctionCodeSigningConfig",
		Handler: func(ctx context.Context, d Deps, c *call) (reply, error) {
			return d.getFunctionCodeSigningConfig(ctx, c)
		}},
	{Method: "POST", Pattern: "/2015-03-31/functions/{FunctionName}/invocations", Action: "Invoke",
		Handler: func(ctx context.Context, d Deps, c *call) (reply, error) {
			return d.invoke(ctx, c)
		}},
	{Method: "POST", Pattern: "/2015-03-31/event-source-mappings/", Action: "CreateEventSourceMapping",
		Handler: func(ctx context.Context, d Deps, c *call) (reply, error) {
			return d.createEventSourceMapping(ctx, c)
		}},
	{Method: "POST", Pattern: "/2015-03-31/event-source-mappings", Action: "CreateEventSourceMapping",
		Handler: func(ctx context.Context, d Deps, c *call) (reply, error) {
			return d.createEventSourceMapping(ctx, c)
		}},
	{Method: "GET", Pattern: "/2015-03-31/event-source-mappings/", Action: "ListEventSourceMappings",
		Handler: func(ctx context.Context, d Deps, c *call) (reply, error) {
			return d.listEventSourceMappings(ctx, c)
		}},
	{Method: "GET", Pattern: "/2015-03-31/event-source-mappings", Action: "ListEventSourceMappings",
		Handler: func(ctx context.Context, d Deps, c *call) (reply, error) {
			return d.listEventSourceMappings(ctx, c)
		}},
	{Method: "GET", Pattern: "/2015-03-31/event-source-mappings/{UUID}", Action: "GetEventSourceMapping",
		Handler: func(ctx context.Context, d Deps, c *call) (reply, error) {
			return d.getEventSourceMapping(ctx, c)
		}},
	{Method: "DELETE", Pattern: "/2015-03-31/event-source-mappings/{UUID}", Action: "DeleteEventSourceMapping",
		Handler: func(ctx context.Context, d Deps, c *call) (reply, error) {
			return d.deleteEventSourceMapping(ctx, c)
		}},
}

var router = rest.NewRouter("lambda", routes)
