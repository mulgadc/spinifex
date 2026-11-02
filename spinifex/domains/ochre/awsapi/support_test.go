//test:in-package — drives the unexported per-service dispatchers.

package awsapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"

	"github.com/mulgadc/bluebottle/pkg/iampolicy"
	"github.com/mulgadc/spinifex/spinifex/ingress/aws/dispatch"
)

type accountKey struct{}

// testGateway holds the capabilities under test and serves a request to one
// service the way the gateway would after authentication, with every policy
// check permitted.
type testGateway struct {
	Deps

	region string
}

func (g *testGateway) serve(service string, w http.ResponseWriter, r *http.Request) error {
	account, _ := r.Context().Value(accountKey{}).(string)
	return g.dispatch(w, dispatch.Invocation{
		Request:   r,
		AccountID: account,
		Service:   service,
		Region:    g.region,
		Authorize: func(_, _ string, _ []string, _ iampolicy.ConditionKeys) error { return nil },
	})
}

func (g *testGateway) Bedrock_Request(w http.ResponseWriter, r *http.Request) error {
	return g.serve(ServiceName, w, r)
}

func (g *testGateway) BedrockRuntime_Request(w http.ResponseWriter, r *http.Request) error {
	return g.serve(serviceRuntime, w, r)
}

func (g *testGateway) BedrockAgent_Request(w http.ResponseWriter, r *http.Request) error {
	return g.serve(serviceAgent, w, r)
}

func (g *testGateway) BedrockAgentRuntime_Request(w http.ResponseWriter, r *http.Request) error {
	return g.serve(serviceAgentRuntime, w, r)
}

// requestForAccount builds a request whose authenticated account is account.
func requestForAccount(account, method, path, body string) *http.Request {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	return req.WithContext(context.WithValue(req.Context(), accountKey{}, account))
}
