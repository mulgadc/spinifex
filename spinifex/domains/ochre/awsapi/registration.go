// Package awsapi is the Bedrock family's ingress adapter. One registration
// serves the four Bedrock services, which share a signing name.
package awsapi

import (
	"context"
	"errors"
	"io"
	"maps"
	"net/http"
	"slices"
	"strings"

	"github.com/mulgadc/bluebottle/pkg/sigv4"
	"github.com/mulgadc/spinifex/spinifex/domains/ochre"
	ochrevector "github.com/mulgadc/spinifex/spinifex/domains/ochre/vector"
	awserrors "github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/mulgadc/spinifex/spinifex/ingress/aws/dispatch"
)

// ServiceName is the SigV4 credential-scope service Bedrock clients sign for.
const ServiceName = "bedrock"

// The sub-services share ServiceName and are told apart by request path. Each
// is also accepted as a credential scope of its own, and a request signed that
// way is dispatched to that sub-service whatever its path.
const (
	serviceRuntime      = "bedrock-runtime"
	serviceAgent        = "bedrock-agent"
	serviceAgentRuntime = "bedrock-agent-runtime"
)

// Quota meters Bedrock usage per account.
type Quota interface {
	CheckBedrockRPM(accountID string) error
	CheckBedrockTokens(ctx context.Context, accountID string) error
}

// Deps are the composed Ochre capabilities the Bedrock services dispatch to.
// A nil capability takes the fallback its accessor documents.
type Deps struct {
	// Credentials resolves per-account provider API keys. Nil resolves none.
	Credentials ochre.CredentialResolver
	// Endpoints resolves self-host model endpoints. Nil resolves none.
	Endpoints ochre.EndpointResolver
	// Access resolves model-access grants. Nil denies every model.
	Access ochre.AccessResolver
	// Recorder records invocations. Nil discards them.
	Recorder ochre.Recorder
	// Logging, Provisioned and Guardrails persist control-plane records. Nil
	// answers every request that reaches them with an error.
	Logging     *ochre.LoggingConfigStore
	Provisioned *ochre.ProvisionedStore
	Guardrails  *ochre.GuardrailStore
	// Embedder drives guardrail topic matching. Nil matches literally.
	Embedder ochre.Embedder
	// KB, DataSources and Vector back the knowledge-base services. Any nil
	// fails those services with ServerInternal.
	KB          *ochrevector.KBStore
	DataSources *ochrevector.DataSourceStore
	Vector      ochrevector.VectorService
	// Quota meters runtime requests. Nil meters nothing.
	Quota Quota
}

// NewRegistration returns the Bedrock registration: AWS JSON 1.1 errors, with
// the three sub-service names as further credential scopes.
func NewRegistration(deps Deps) dispatch.Registration {
	return dispatch.Registration{
		Service:       ServiceName,
		Aliases:       []string{serviceRuntime, serviceAgent, serviceAgentRuntime},
		ResolveAction: resolveAction,
		Dispatch:      deps.dispatch,
		Errors:        dispatch.ErrorEnvelopeJSON,
		Inventory:     OperationInventory(),
	}
}

// OperationInventory classifies the actions the registration serves.
func OperationInventory() dispatch.Inventory {
	names := map[string]struct{}{}
	for _, routes := range [][]string{
		actionsOf(bedrockRoutes, func(r bedrockRoute) string { return r.Action }),
		actionsOf(bedrockRuntimeRoutes, func(r bedrockRuntimeRoute) string { return r.Action }),
		actionsOf(bedrockAgentRoutes, func(r bedrockAgentRoute) string { return r.Action }),
		actionsOf(bedrockAgentRuntimeRoutes, func(r bedrockAgentRuntimeRoute) string { return r.Action }),
	} {
		for _, n := range routes {
			names[n] = struct{}{}
		}
	}
	return dispatch.Inventory{Registered: slices.Sorted(maps.Keys(names))}
}

func actionsOf[T any](routes []T, action func(T) string) []string {
	actions := make([]string, 0, len(routes))
	for _, route := range routes {
		actions = append(actions, action(route))
	}
	return actions
}

// resolveAction labels a request by the path-selected sub-service. It is
// advisory: a request signed with a sub-service scope may be labelled for a
// different sub-service than the one that serves it.
func resolveAction(r *http.Request) string {
	method, path := r.Method, r.URL.EscapedPath()
	var action string
	var ok bool
	switch resolveSubservice(r.URL.Path) {
	case serviceRuntime:
		action, _, _, ok = bedrockRuntimeRouter.Lookup(method, path)
	case serviceAgentRuntime:
		action, _, _, ok = bedrockAgentRuntimeRouter.Lookup(method, path)
	case serviceAgent:
		action, _, _, ok = bedrockAgentRouter.Lookup(method, path)
	default:
		action, _, _, ok = bedrockRouter.Lookup(method, path)
	}
	if !ok {
		return ""
	}
	return action
}

// resolveSubservice maps a request path to the sub-service that serves it.
func resolveSubservice(path string) string {
	switch {
	case strings.HasPrefix(path, "/model/") || strings.HasPrefix(path, "/guardrail/"):
		return serviceRuntime
	case path == "/retrieveAndGenerate" || (strings.HasPrefix(path, "/knowledgebases/") && strings.HasSuffix(path, "/retrieve")):
		return serviceAgentRuntime
	case strings.HasPrefix(path, "/knowledgebases/"):
		return serviceAgent
	default:
		return ServiceName
	}
}

// dispatch hands the request to its sub-service. Only the shared signing name
// is split by path; a sub-service scope selects that sub-service outright.
func (d Deps) dispatch(w http.ResponseWriter, inv dispatch.Invocation) error {
	sub := inv.Service
	switch sub {
	case serviceRuntime, serviceAgent, serviceAgentRuntime:
	default:
		sub = resolveSubservice(inv.Request.URL.Path)
	}
	switch sub {
	case serviceRuntime:
		return d.bedrockRuntime(w, inv)
	case serviceAgent:
		return d.bedrockAgent(w, inv)
	case serviceAgentRuntime:
		return d.bedrockAgentRuntime(w, inv)
	default:
		return d.bedrock(w, inv)
	}
}

func readBoundedBody(r *http.Request) ([]byte, error) {
	body, err := io.ReadAll(io.LimitReader(r.Body, sigv4.MaxPayloadLen+1))
	if err != nil {
		return nil, errors.New(awserrors.ErrorInvalidParameterValue)
	}
	if int64(len(body)) > sigv4.MaxPayloadLen {
		return nil, errors.New(awserrors.ErrorRequestEntityTooLarge)
	}
	return body, nil
}

func (d Deps) resolver() ochre.CredentialResolver {
	if d.Credentials != nil {
		return d.Credentials
	}
	return ochre.NoopCredentialResolver
}

func (d Deps) loggingStore() *ochre.LoggingConfigStore {
	if d.Logging != nil {
		return d.Logging
	}
	return ochre.NewLoggingConfigStore(nil)
}

func (d Deps) recorder() ochre.Recorder {
	if d.Recorder != nil {
		return d.Recorder
	}
	return ochre.NoopRecorder
}

// accessResolver wraps a configured grant store so a staged self-host model is
// granted to every account. Without one every model is denied, unwrapped.
func (d Deps) accessResolver() ochre.AccessResolver {
	if d.Access != nil {
		return ochre.NewStagedOpenAccessResolver(d.Access)
	}
	return ochre.DenyAllAccessResolver
}

func (d Deps) provisionedStore(region string) *ochre.ProvisionedStore {
	if d.Provisioned != nil {
		return d.Provisioned
	}
	return ochre.NewProvisionedStore(nil, region, nil)
}

func (d Deps) guardrailStore(region string) *ochre.GuardrailStore {
	if d.Guardrails != nil {
		return d.Guardrails
	}
	return ochre.NewGuardrailStore(nil, region)
}

func (d Deps) endpointResolver() ochre.EndpointResolver {
	if d.Endpoints != nil {
		return d.Endpoints
	}
	return ochre.NewStaticEndpointResolver(nil)
}

func (d Deps) checkRPM(accountID string) error {
	if d.Quota == nil {
		return nil
	}
	return d.Quota.CheckBedrockRPM(accountID)
}

func (d Deps) checkTokens(ctx context.Context, accountID string) error {
	if d.Quota == nil {
		return nil
	}
	return d.Quota.CheckBedrockTokens(ctx, accountID)
}
