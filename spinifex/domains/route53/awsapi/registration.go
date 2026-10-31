// Package awsapi adapts Route 53's REST-XML API to the route53 domain's
// NATS-backed service. Generic gateway routing and authorization stay at the
// ingress boundary.
package awsapi

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go/service/route53"
	"github.com/mulgadc/bluebottle/pkg/sigv4"
	"github.com/mulgadc/spinifex/spinifex/domains/route53/ext"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	natsmsg "github.com/mulgadc/spinifex/spinifex/foundation/messaging/nats"
	"github.com/mulgadc/spinifex/spinifex/ingress/aws/dispatch"
	"github.com/mulgadc/spinifex/spinifex/ingress/aws/rest"
	"github.com/nats-io/nats.go"
)

// ServiceName is the SigV4 credential-scope service Route 53 clients sign for.
const ServiceName = "route53"

// natsTimeout bounds the gateway's wait for the daemon's answer.
const natsTimeout = 30 * time.Second

// Deps are the composed capabilities the Route 53 control plane dispatches
// to. A nil NATS connection answers every action with ServerInternal.
type Deps struct {
	NATS *nats.Conn
}

// operation is one Route 53 action: its wire input and output types and the
// status a success returns.
type operation struct {
	newInput func() any
	relay    func(ctx context.Context, nc *nats.Conn, subject string, in any, account string) (any, error)
	status   int
}

func op[I, O any](status int) operation {
	return operation{
		newInput: func() any { return new(I) },
		relay: func(ctx context.Context, nc *nats.Conn, subject string, in any, account string) (any, error) {
			return natsmsg.NATSRequest[O](ctx, nc, subject, in, natsTimeout, account)
		},
		status: status,
	}
}

const base = "/2013-04-01"

// routes is the Route 53 REST surface Spinifex serves, paths as in the
// 2013-04-01 model.
var routes = []rest.Route[operation]{
	{Method: http.MethodPost, Pattern: base + "/hostedzone", Action: "CreateHostedZone", Handler: op[route53.CreateHostedZoneInput, route53.CreateHostedZoneOutput](http.StatusCreated)},
	{Method: http.MethodGet, Pattern: base + "/hostedzone", Action: "ListHostedZones", Handler: op[route53.ListHostedZonesInput, route53.ListHostedZonesOutput](http.StatusOK)},
	{Method: http.MethodGet, Pattern: base + "/hostedzonesbyname", Action: "ListHostedZonesByName", Handler: op[route53.ListHostedZonesByNameInput, route53.ListHostedZonesByNameOutput](http.StatusOK)},
	{Method: http.MethodGet, Pattern: base + "/hostedzonecount", Action: "GetHostedZoneCount", Handler: op[route53.GetHostedZoneCountInput, route53.GetHostedZoneCountOutput](http.StatusOK)},
	{Method: http.MethodGet, Pattern: base + "/hostedzone/{Id}", Action: "GetHostedZone", Handler: op[route53.GetHostedZoneInput, route53.GetHostedZoneOutput](http.StatusOK)},
	{Method: http.MethodPost, Pattern: base + "/hostedzone/{Id}", Action: "UpdateHostedZoneComment", Handler: op[route53.UpdateHostedZoneCommentInput, route53.UpdateHostedZoneCommentOutput](http.StatusOK)},
	{Method: http.MethodDelete, Pattern: base + "/hostedzone/{Id}", Action: "DeleteHostedZone", Handler: op[route53.DeleteHostedZoneInput, route53.DeleteHostedZoneOutput](http.StatusOK)},
	{Method: http.MethodPost, Pattern: base + "/hostedzone/{Id}/rrset", Action: "ChangeResourceRecordSets", Handler: op[route53.ChangeResourceRecordSetsInput, route53.ChangeResourceRecordSetsOutput](http.StatusOK)},
	{Method: http.MethodGet, Pattern: base + "/hostedzone/{Id}/rrset", Action: "ListResourceRecordSets", Handler: op[route53.ListResourceRecordSetsInput, route53.ListResourceRecordSetsOutput](http.StatusOK)},
	{Method: http.MethodGet, Pattern: base + "/change/{Id}", Action: "GetChange", Handler: op[route53.GetChangeInput, route53.GetChangeOutput](http.StatusOK)},
	{Method: http.MethodPost, Pattern: base + "/tags/{ResourceType}/{ResourceId}", Action: "ChangeTagsForResource", Handler: op[route53.ChangeTagsForResourceInput, route53.ChangeTagsForResourceOutput](http.StatusOK)},
	{Method: http.MethodGet, Pattern: base + "/tags/{ResourceType}/{ResourceId}", Action: "ListTagsForResource", Handler: op[route53.ListTagsForResourceInput, route53.ListTagsForResourceOutput](http.StatusOK)},
	{Method: http.MethodPost, Pattern: base + "/tags/{ResourceType}", Action: "ListTagsForResources", Handler: op[route53.ListTagsForResourcesInput, route53.ListTagsForResourcesOutput](http.StatusOK)},

	// Spinifex extensions, which standard SDKs never call.
	{Method: http.MethodGet, Pattern: base + "/hostedzone/{Id}/activation", Action: ext.ActionGetHostedZoneActivation, Handler: op[ext.GetHostedZoneActivationInput, ext.GetHostedZoneActivationOutput](http.StatusOK)},
	{Method: http.MethodPost, Pattern: base + "/hostedzone/{Id}/activation", Action: ext.ActionVerifyHostedZoneDelegation, Handler: op[ext.VerifyHostedZoneDelegationInput, ext.VerifyHostedZoneDelegationOutput](http.StatusOK)},
}

// extensions are served but are not operations of the AWS model.
var extensions = []string{ext.ActionGetHostedZoneActivation, ext.ActionVerifyHostedZoneDelegation}

var router = rest.NewRouter(ServiceName, routes)

// NewRegistration returns the Route 53 REST-XML control-plane registration.
func NewRegistration(deps Deps) dispatch.Registration {
	return dispatch.Registration{
		Service:       ServiceName,
		ResolveAction: resolveAction,
		Dispatch:      deps.dispatch,
		Errors:        dispatch.ErrorEnvelopeXML,
		Traits:        dispatch.ServiceTraits{XMLThrottleCode: awserrors.ErrorThrottling},
		Inventory:     OperationInventory(),
	}
}

// ExtensionActions lists the Spinifex actions served beside the AWS model's.
func ExtensionActions() []string { return slices.Clone(extensions) }

// OperationInventory lists the AWS model actions the registration serves.
func OperationInventory() dispatch.Inventory {
	names := make([]string, 0, len(routes))
	for _, r := range routes {
		if !slices.Contains(extensions, r.Action) {
			names = append(names, r.Action)
		}
	}
	slices.Sort(names)
	return dispatch.Inventory{Registered: names}
}

func resolveAction(r *http.Request) string {
	action, _, _, _ := lookup(r)
	return action
}

// lookup matches a request to its route. The model's ChangeResourceRecordSets
// path ends in a slash, which SDKs send and the router does not match.
func lookup(r *http.Request) (string, []string, operation, bool) {
	return router.Lookup(r.Method, strings.TrimSuffix(r.URL.EscapedPath(), "/"))
}

// dispatch binds the request, authorizes it against the zone or change it
// names, relays it to the daemon and writes the XML response.
func (d Deps) dispatch(w http.ResponseWriter, inv dispatch.Invocation) error {
	r := inv.Request
	action, params, operation, ok := lookup(r)
	if !ok {
		slog.DebugContext(r.Context(), "Route 53: no route", "method", r.Method, "path", r.URL.Path)
		return errors.New(awserrors.ErrorInvalidAction)
	}
	if inv.AccountID == "" {
		slog.ErrorContext(r.Context(), "Route 53 dispatch: no account ID in auth context")
		return errors.New(awserrors.ErrorInternalError)
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, sigv4.MaxPayloadLen+1))
	if err != nil {
		return errors.New(awserrors.ErrorInvalidInput)
	}
	if int64(len(body)) > sigv4.MaxPayloadLen {
		return errors.New(awserrors.ErrorRequestEntityTooLarge)
	}

	input := operation.newInput()
	if err := bindInput(input, pathParams(action, params), r.URL.Query(), body); err != nil {
		return err
	}
	if err := inv.Authorize(ServiceName, action, ResourceARNs(input), nil); err != nil {
		return err
	}
	if d.NATS == nil {
		return errors.New(awserrors.ErrorServerInternal)
	}
	output, err := operation.relay(r.Context(), d.NATS, ServiceName+"."+action, input, inv.AccountID)
	if err != nil {
		return err
	}
	return writeXML(w, r, action, operation.status, output)
}

// pathParams names the router's positional params by the route's pattern.
func pathParams(action string, params []string) map[string]string {
	for _, rt := range routes {
		if rt.Action != action {
			continue
		}
		names := patternNames(rt.Pattern)
		out := make(map[string]string, len(names))
		for i, n := range names {
			if i < len(params) {
				out[n] = params[i]
			}
		}
		return out
	}
	return nil
}
