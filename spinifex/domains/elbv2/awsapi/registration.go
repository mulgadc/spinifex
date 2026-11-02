package awsapi

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"maps"
	"net/http"
	"slices"

	"github.com/aws/aws-sdk-go/service/elbv2"
	"github.com/mulgadc/bluebottle/pkg/sigv4"
	elbv2domain "github.com/mulgadc/spinifex/spinifex/domains/elbv2"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	awsxml "github.com/mulgadc/spinifex/spinifex/foundation/aws/xml"
	"github.com/mulgadc/spinifex/spinifex/ingress/aws/dispatch"
	"github.com/mulgadc/spinifex/spinifex/ingress/aws/query"
	"github.com/nats-io/nats.go"
)

// ServiceName is the SigV4 credential-scope service ELBv2 clients sign for.
const ServiceName = "elasticloadbalancing"

// InventoryName keys the operation inventory; it differs from the credential
// scope because the inventory is named for the API generation.
const InventoryName = "elasticloadbalancingv2"

// Quota is the account load balancer cap ELBv2 consults. It is declared here
// because domains/admission/quota imports this package to count live load
// balancers, so this package cannot import it back.
type Quota interface {
	EnforceLoadBalancers(ctx context.Context, nc *nats.Conn, accountID string, want int) error
	Exempt(accountID string) bool
	// LoadBalancerLimit is the cap the account's DescribeAccountLimits reports.
	LoadBalancerLimit(ctx context.Context, accountID string) (int, error)
}

// Deps are the composed capabilities the ELBv2 control plane dispatches to. A
// nil NATS connection answers every action with ServerInternal and a nil Quota
// enforces and reports the AWS default.
type Deps struct {
	NATS  *nats.Conn
	Quota Quota
}

// action parses once so authorization and dispatch share one typed input.
type action struct {
	parse    func(map[string]string) (any, error)
	dispatch func(ctx context.Context, name string, parsed any, d Deps, accountID string) ([]byte, error)
}

// handler creates a type-safe action. Parsing is separate from dispatch so the
// resolver authorizes the exact input the handler executes. ELBv2 uses the
// IAM-style XML envelope: <ActionResponse><ActionResult>...</ActionResult></ActionResponse>.
func handler[In any](h func(context.Context, *In, Deps, string) (any, error)) action {
	return action{
		parse: func(q map[string]string) (any, error) {
			input := new(In)
			if err := query.QueryParamsToStruct(q, input); err != nil {
				return nil, err
			}
			return input, nil
		},
		dispatch: func(ctx context.Context, name string, parsed any, d Deps, accountID string) ([]byte, error) {
			input, ok := parsed.(*In)
			if !ok {
				return nil, errors.New(awserrors.ErrorInternalError)
			}
			output, err := h(ctx, input, d, accountID)
			if err != nil {
				return nil, err
			}
			payload := awsxml.QueryResponsePayload(name, output)
			xmlOutput, err := awsxml.Marshal(payload)
			if err != nil {
				return nil, errors.New("failed to marshal response to XML")
			}
			return xmlOutput, nil
		},
	}
}

var actions = map[string]action{
	"CreateLoadBalancer": handler(func(ctx context.Context, input *elbv2.CreateLoadBalancerInput, d Deps, accountID string) (any, error) {
		if d.Quota != nil {
			if err := d.Quota.EnforceLoadBalancers(ctx, d.NATS, accountID, 1); err != nil {
				return nil, err
			}
		}
		return CreateLoadBalancer(ctx, input, d.NATS, accountID)
	}),
	"DeleteLoadBalancer": handler(func(ctx context.Context, input *elbv2.DeleteLoadBalancerInput, d Deps, accountID string) (any, error) {
		return DeleteLoadBalancer(ctx, input, d.NATS, accountID)
	}),
	"DescribeLoadBalancers": handler(func(ctx context.Context, input *elbv2.DescribeLoadBalancersInput, d Deps, accountID string) (any, error) {
		return DescribeLoadBalancers(ctx, input, d.NATS, accountID)
	}),
	"CreateTargetGroup": handler(func(ctx context.Context, input *elbv2.CreateTargetGroupInput, d Deps, accountID string) (any, error) {
		return CreateTargetGroup(ctx, input, d.NATS, accountID)
	}),
	"DeleteTargetGroup": handler(func(ctx context.Context, input *elbv2.DeleteTargetGroupInput, d Deps, accountID string) (any, error) {
		return DeleteTargetGroup(ctx, input, d.NATS, accountID)
	}),
	"DescribeTargetGroups": handler(func(ctx context.Context, input *elbv2.DescribeTargetGroupsInput, d Deps, accountID string) (any, error) {
		return DescribeTargetGroups(ctx, input, d.NATS, accountID)
	}),
	"RegisterTargets": handler(func(ctx context.Context, input *elbv2.RegisterTargetsInput, d Deps, accountID string) (any, error) {
		return RegisterTargets(ctx, input, d.NATS, accountID)
	}),
	"DeregisterTargets": handler(func(ctx context.Context, input *elbv2.DeregisterTargetsInput, d Deps, accountID string) (any, error) {
		return DeregisterTargets(ctx, input, d.NATS, accountID)
	}),
	"DescribeTargetHealth": handler(func(ctx context.Context, input *elbv2.DescribeTargetHealthInput, d Deps, accountID string) (any, error) {
		return DescribeTargetHealth(ctx, input, d.NATS, accountID)
	}),
	"CreateListener": handler(func(ctx context.Context, input *elbv2.CreateListenerInput, d Deps, accountID string) (any, error) {
		return CreateListener(ctx, input, d.NATS, accountID)
	}),
	"DeleteListener": handler(func(ctx context.Context, input *elbv2.DeleteListenerInput, d Deps, accountID string) (any, error) {
		return DeleteListener(ctx, input, d.NATS, accountID)
	}),
	"ModifyListener": handler(func(ctx context.Context, input *elbv2.ModifyListenerInput, d Deps, accountID string) (any, error) {
		return ModifyListener(ctx, input, d.NATS, accountID)
	}),
	"DescribeListeners": handler(func(ctx context.Context, input *elbv2.DescribeListenersInput, d Deps, accountID string) (any, error) {
		return DescribeListeners(ctx, input, d.NATS, accountID)
	}),
	"CreateRule": handler(func(ctx context.Context, input *elbv2.CreateRuleInput, d Deps, accountID string) (any, error) {
		return CreateRule(ctx, input, d.NATS, accountID)
	}),
	"ModifyRule": handler(func(ctx context.Context, input *elbv2.ModifyRuleInput, d Deps, accountID string) (any, error) {
		return ModifyRule(ctx, input, d.NATS, accountID)
	}),
	"DeleteRule": handler(func(ctx context.Context, input *elbv2.DeleteRuleInput, d Deps, accountID string) (any, error) {
		return DeleteRule(ctx, input, d.NATS, accountID)
	}),
	"DescribeRules": handler(func(ctx context.Context, input *elbv2.DescribeRulesInput, d Deps, accountID string) (any, error) {
		return DescribeRules(ctx, input, d.NATS, accountID)
	}),
	"SetRulePriorities": handler(func(ctx context.Context, input *elbv2.SetRulePrioritiesInput, d Deps, accountID string) (any, error) {
		return SetRulePriorities(ctx, input, d.NATS, accountID)
	}),
	"DescribeTags": handler(func(ctx context.Context, input *elbv2.DescribeTagsInput, d Deps, accountID string) (any, error) {
		return DescribeTags(ctx, input, d.NATS, accountID)
	}),
	"AddTags": handler(func(ctx context.Context, input *elbv2.AddTagsInput, d Deps, accountID string) (any, error) {
		return AddTags(ctx, input, d.NATS, accountID)
	}),
	"RemoveTags": handler(func(ctx context.Context, input *elbv2.RemoveTagsInput, d Deps, accountID string) (any, error) {
		return RemoveTags(ctx, input, d.NATS, accountID)
	}),
	"LBAgentHeartbeat": handler(func(ctx context.Context, input *elbv2domain.LBAgentHeartbeatInput, d Deps, accountID string) (any, error) {
		return LBAgentHeartbeat(ctx, input, d.NATS, accountID)
	}),
	"GetLBConfig": handler(func(ctx context.Context, input *elbv2domain.GetLBConfigInput, d Deps, accountID string) (any, error) {
		return GetLBConfig(ctx, input, d.NATS, accountID)
	}),
	"ModifyTargetGroup": handler(func(ctx context.Context, input *elbv2.ModifyTargetGroupInput, d Deps, accountID string) (any, error) {
		return ModifyTargetGroup(ctx, input, d.NATS, accountID)
	}),
	"ModifyTargetGroupAttributes": handler(func(ctx context.Context, input *elbv2.ModifyTargetGroupAttributesInput, d Deps, accountID string) (any, error) {
		return ModifyTargetGroupAttributes(ctx, input, d.NATS, accountID)
	}),
	"DescribeTargetGroupAttributes": handler(func(ctx context.Context, input *elbv2.DescribeTargetGroupAttributesInput, d Deps, accountID string) (any, error) {
		return DescribeTargetGroupAttributes(ctx, input, d.NATS, accountID)
	}),
	"ModifyLoadBalancerAttributes": handler(func(ctx context.Context, input *elbv2.ModifyLoadBalancerAttributesInput, d Deps, accountID string) (any, error) {
		return ModifyLoadBalancerAttributes(ctx, input, d.NATS, accountID)
	}),
	"DescribeLoadBalancerAttributes": handler(func(ctx context.Context, input *elbv2.DescribeLoadBalancerAttributesInput, d Deps, accountID string) (any, error) {
		return DescribeLoadBalancerAttributes(ctx, input, d.NATS, accountID)
	}),
	"SetSecurityGroups": handler(func(ctx context.Context, input *elbv2.SetSecurityGroupsInput, d Deps, accountID string) (any, error) {
		return SetSecurityGroups(ctx, input, d.NATS, accountID)
	}),
	"SetIpAddressType": handler(func(ctx context.Context, input *elbv2.SetIpAddressTypeInput, d Deps, accountID string) (any, error) {
		return SetIpAddressType(ctx, input, d.NATS, accountID)
	}),
	"SetSubnets": handler(func(ctx context.Context, input *elbv2.SetSubnetsInput, d Deps, accountID string) (any, error) {
		return SetSubnets(ctx, input, d.NATS, accountID)
	}),
	"AddListenerCertificates": handler(func(ctx context.Context, input *elbv2.AddListenerCertificatesInput, d Deps, accountID string) (any, error) {
		return AddListenerCertificates(ctx, input, d.NATS, accountID)
	}),
	"RemoveListenerCertificates": handler(func(ctx context.Context, input *elbv2.RemoveListenerCertificatesInput, d Deps, accountID string) (any, error) {
		return RemoveListenerCertificates(ctx, input, d.NATS, accountID)
	}),
	"DescribeListenerCertificates": handler(func(ctx context.Context, input *elbv2.DescribeListenerCertificatesInput, d Deps, accountID string) (any, error) {
		return DescribeListenerCertificates(ctx, input, d.NATS, accountID)
	}),
	"DescribeSSLPolicies": handler(func(ctx context.Context, input *elbv2.DescribeSSLPoliciesInput, d Deps, accountID string) (any, error) {
		return DescribeSSLPolicies(ctx, input, d.NATS, accountID)
	}),
	"DescribeAccountLimits": handler(func(ctx context.Context, input *elbv2.DescribeAccountLimitsInput, d Deps, accountID string) (any, error) {
		loadBalancers := DefaultLoadBalancerLimit
		if d.Quota != nil {
			limit, err := d.Quota.LoadBalancerLimit(ctx, accountID)
			if err != nil {
				return nil, err
			}
			loadBalancers = limit
		}
		return DescribeAccountLimits(input, loadBalancers), nil
	}),
	"DescribeListenerAttributes": handler(func(ctx context.Context, input *DescribeListenerAttributesInput, d Deps, accountID string) (any, error) {
		return DescribeListenerAttributes(input, accountID)
	}),
	"ModifyListenerAttributes": handler(func(ctx context.Context, input *ModifyListenerAttributesInput, d Deps, accountID string) (any, error) {
		return ModifyListenerAttributes(input, accountID)
	}),
}

// NewRegistration returns the ELBv2 AWS Query control-plane registration.
func NewRegistration(deps Deps) dispatch.Registration {
	return dispatch.Registration{
		Service:       ServiceName,
		InventoryName: InventoryName,
		ResolveAction: resolveAction,
		Dispatch:      deps.dispatch,
		Errors:        dispatch.ErrorEnvelopeXML,
		Inventory:     OperationInventory(),
	}
}

// ActionNames lists every action the registration dispatches, sorted.
func ActionNames() []string {
	return slices.Sorted(maps.Keys(actions))
}

// OperationInventory classifies the actions the registration serves. Every
// ELBv2 action is implemented, so none is Stubbed or Unsupported.
func OperationInventory() dispatch.Inventory {
	return dispatch.Inventory{Registered: ActionNames()}
}

// resolveAction is the one Query Action parser telemetry and dispatch share.
// It leaves the body readable for the dispatcher and answers "" on any failure.
func resolveAction(r *http.Request) string {
	args, err := readQueryArgs(r)
	if err != nil {
		return ""
	}
	return args["Action"]
}

// readQueryArgs parses the Query body and rewinds it, so the same bytes can be
// read again. Any read or parse failure is a malformed query string.
func readQueryArgs(r *http.Request) (map[string]string, error) {
	body, err := io.ReadAll(io.LimitReader(r.Body, sigv4.MaxPayloadLen+1))
	if err != nil {
		return nil, err
	}
	r.Body = io.NopCloser(bytes.NewReader(body))
	if int64(len(body)) > sigv4.MaxPayloadLen {
		return nil, errors.New(awserrors.ErrorRequestEntityTooLarge)
	}
	return query.ParseArgs(string(body))
}

// dispatch parses, scopes and authorizes the request from one parse of its
// body, then runs the action. Errors are awserrors codes the gateway renders.
func (d Deps) dispatch(w http.ResponseWriter, inv dispatch.Invocation) error {
	r := inv.Request
	queryArgs, err := readQueryArgs(r)
	if err != nil {
		slog.DebugContext(r.Context(), "ELBv2: malformed query string", "err", err)
		return errors.New(awserrors.ErrorMalformedQueryString)
	}

	name := queryArgs["Action"]
	if name == "" {
		return errors.New(awserrors.ErrorMissingAction)
	}
	h, ok := actions[name]
	if !ok {
		return errors.New(awserrors.ErrorInvalidAction)
	}

	// inv.Region is the gateway's own region, never the caller-supplied
	// credential-scope region, which would let a caller sign for another region
	// and slide out from under a Deny.
	accountID := inv.AccountID
	if accountID == "" {
		slog.ErrorContext(r.Context(), "ELBv2 dispatch: no account ID in auth context")
		// InternalError, not ServerInternal: the policy gate used to reach this
		// case first and that is the code the caller has always seen.
		return errors.New(awserrors.ErrorInternalError)
	}

	input, err := h.parse(queryArgs)
	if err != nil {
		if errors.Is(err, query.ErrSliceTooLarge) {
			return errors.New(awserrors.ErrorMalformedQueryString)
		}
		return err
	}

	resources, err := ResourceARNs(name, inv.Region, accountID, input)
	if err != nil {
		return err
	}
	if err := inv.Authorize(ServiceName, name, resources, nil); err != nil {
		return err
	}

	if d.NATS == nil {
		return errors.New(awserrors.ErrorServerInternal)
	}

	xmlOutput, err := h.dispatch(r.Context(), name, input, d, accountID)
	if err != nil {
		return err
	}

	w.Header().Set("Content-Type", "text/xml")
	w.WriteHeader(http.StatusOK)
	if _, err := w.Write(xmlOutput); err != nil {
		slog.ErrorContext(r.Context(), "Failed to write ELBv2 response", "err", err)
	}
	return nil
}
