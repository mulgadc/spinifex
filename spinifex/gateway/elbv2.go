package gateway

import (
	"context"
	"errors"
	"log/slog"
	"net/http"

	"github.com/aws/aws-sdk-go/service/elbv2"
	"github.com/mulgadc/spinifex/spinifex/awsec2query"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	gateway_elbv2 "github.com/mulgadc/spinifex/spinifex/gateway/elbv2"
	handlers_elbv2 "github.com/mulgadc/spinifex/spinifex/handlers/elbv2"
	"github.com/mulgadc/spinifex/spinifex/utils"
)

// ELBv2Handler processes parsed query args and returns XML response bytes.
type ELBv2Handler func(ctx context.Context, action string, q map[string]string, gw *GatewayConfig, accountID string) ([]byte, error)

// elbv2Handler creates a type-safe ELBv2Handler that allocates the typed input struct,
// parses query params into it, calls the handler, and marshals the output to XML.
// ELBv2 uses the IAM-style XML envelope: <ActionResponse><ActionResult>...</ActionResult></ActionResponse>
func elbv2Handler[In any](handler func(context.Context, *In, *GatewayConfig, string) (any, error)) ELBv2Handler {
	return func(ctx context.Context, action string, q map[string]string, gw *GatewayConfig, accountID string) ([]byte, error) {
		input := new(In)
		if err := awsec2query.QueryParamsToStruct(q, input); err != nil {
			if errors.Is(err, awsec2query.ErrSliceTooLarge) {
				return nil, errors.New(awserrors.ErrorMalformedQueryString)
			}
			return nil, err
		}
		output, err := handler(ctx, input, gw, accountID)
		if err != nil {
			return nil, err
		}
		payload := utils.GenerateIAMXMLPayload(action, output)
		xmlOutput, err := utils.MarshalToXML(payload)
		if err != nil {
			return nil, errors.New("failed to marshal response to XML")
		}
		return xmlOutput, nil
	}
}

var elbv2Actions = map[string]ELBv2Handler{
	"CreateLoadBalancer": elbv2Handler(func(ctx context.Context, input *elbv2.CreateLoadBalancerInput, gw *GatewayConfig, accountID string) (any, error) {
		return gateway_elbv2.CreateLoadBalancer(ctx, input, gw.NATSConn, accountID)
	}),
	"DeleteLoadBalancer": elbv2Handler(func(ctx context.Context, input *elbv2.DeleteLoadBalancerInput, gw *GatewayConfig, accountID string) (any, error) {
		return gateway_elbv2.DeleteLoadBalancer(ctx, input, gw.NATSConn, accountID)
	}),
	"DescribeLoadBalancers": elbv2Handler(func(ctx context.Context, input *elbv2.DescribeLoadBalancersInput, gw *GatewayConfig, accountID string) (any, error) {
		return gateway_elbv2.DescribeLoadBalancers(ctx, input, gw.NATSConn, accountID)
	}),
	"CreateTargetGroup": elbv2Handler(func(ctx context.Context, input *elbv2.CreateTargetGroupInput, gw *GatewayConfig, accountID string) (any, error) {
		return gateway_elbv2.CreateTargetGroup(ctx, input, gw.NATSConn, accountID)
	}),
	"DeleteTargetGroup": elbv2Handler(func(ctx context.Context, input *elbv2.DeleteTargetGroupInput, gw *GatewayConfig, accountID string) (any, error) {
		return gateway_elbv2.DeleteTargetGroup(ctx, input, gw.NATSConn, accountID)
	}),
	"DescribeTargetGroups": elbv2Handler(func(ctx context.Context, input *elbv2.DescribeTargetGroupsInput, gw *GatewayConfig, accountID string) (any, error) {
		return gateway_elbv2.DescribeTargetGroups(ctx, input, gw.NATSConn, accountID)
	}),
	"RegisterTargets": elbv2Handler(func(ctx context.Context, input *elbv2.RegisterTargetsInput, gw *GatewayConfig, accountID string) (any, error) {
		return gateway_elbv2.RegisterTargets(ctx, input, gw.NATSConn, accountID)
	}),
	"DeregisterTargets": elbv2Handler(func(ctx context.Context, input *elbv2.DeregisterTargetsInput, gw *GatewayConfig, accountID string) (any, error) {
		return gateway_elbv2.DeregisterTargets(ctx, input, gw.NATSConn, accountID)
	}),
	"DescribeTargetHealth": elbv2Handler(func(ctx context.Context, input *elbv2.DescribeTargetHealthInput, gw *GatewayConfig, accountID string) (any, error) {
		return gateway_elbv2.DescribeTargetHealth(ctx, input, gw.NATSConn, accountID)
	}),
	"CreateListener": elbv2Handler(func(ctx context.Context, input *elbv2.CreateListenerInput, gw *GatewayConfig, accountID string) (any, error) {
		return gateway_elbv2.CreateListener(ctx, input, gw.NATSConn, accountID)
	}),
	"DeleteListener": elbv2Handler(func(ctx context.Context, input *elbv2.DeleteListenerInput, gw *GatewayConfig, accountID string) (any, error) {
		return gateway_elbv2.DeleteListener(ctx, input, gw.NATSConn, accountID)
	}),
	"ModifyListener": elbv2Handler(func(ctx context.Context, input *elbv2.ModifyListenerInput, gw *GatewayConfig, accountID string) (any, error) {
		return gateway_elbv2.ModifyListener(ctx, input, gw.NATSConn, accountID)
	}),
	"DescribeListeners": elbv2Handler(func(ctx context.Context, input *elbv2.DescribeListenersInput, gw *GatewayConfig, accountID string) (any, error) {
		return gateway_elbv2.DescribeListeners(ctx, input, gw.NATSConn, accountID)
	}),
	"CreateRule": elbv2Handler(func(ctx context.Context, input *elbv2.CreateRuleInput, gw *GatewayConfig, accountID string) (any, error) {
		return gateway_elbv2.CreateRule(ctx, input, gw.NATSConn, accountID)
	}),
	"ModifyRule": elbv2Handler(func(ctx context.Context, input *elbv2.ModifyRuleInput, gw *GatewayConfig, accountID string) (any, error) {
		return gateway_elbv2.ModifyRule(ctx, input, gw.NATSConn, accountID)
	}),
	"DeleteRule": elbv2Handler(func(ctx context.Context, input *elbv2.DeleteRuleInput, gw *GatewayConfig, accountID string) (any, error) {
		return gateway_elbv2.DeleteRule(ctx, input, gw.NATSConn, accountID)
	}),
	"DescribeRules": elbv2Handler(func(ctx context.Context, input *elbv2.DescribeRulesInput, gw *GatewayConfig, accountID string) (any, error) {
		return gateway_elbv2.DescribeRules(ctx, input, gw.NATSConn, accountID)
	}),
	"SetRulePriorities": elbv2Handler(func(ctx context.Context, input *elbv2.SetRulePrioritiesInput, gw *GatewayConfig, accountID string) (any, error) {
		return gateway_elbv2.SetRulePriorities(ctx, input, gw.NATSConn, accountID)
	}),
	"DescribeTags": elbv2Handler(func(ctx context.Context, input *elbv2.DescribeTagsInput, gw *GatewayConfig, accountID string) (any, error) {
		return gateway_elbv2.DescribeTags(ctx, input, gw.NATSConn, accountID)
	}),
	"AddTags": elbv2Handler(func(ctx context.Context, input *elbv2.AddTagsInput, gw *GatewayConfig, accountID string) (any, error) {
		return gateway_elbv2.AddTags(ctx, input, gw.NATSConn, accountID)
	}),
	"RemoveTags": elbv2Handler(func(ctx context.Context, input *elbv2.RemoveTagsInput, gw *GatewayConfig, accountID string) (any, error) {
		return gateway_elbv2.RemoveTags(ctx, input, gw.NATSConn, accountID)
	}),
	"LBAgentHeartbeat": elbv2Handler(func(ctx context.Context, input *handlers_elbv2.LBAgentHeartbeatInput, gw *GatewayConfig, accountID string) (any, error) {
		return gateway_elbv2.LBAgentHeartbeat(ctx, input, gw.NATSConn, accountID)
	}),
	"GetLBConfig": elbv2Handler(func(ctx context.Context, input *handlers_elbv2.GetLBConfigInput, gw *GatewayConfig, accountID string) (any, error) {
		return gateway_elbv2.GetLBConfig(ctx, input, gw.NATSConn, accountID)
	}),
	"ModifyTargetGroup": elbv2Handler(func(ctx context.Context, input *elbv2.ModifyTargetGroupInput, gw *GatewayConfig, accountID string) (any, error) {
		return gateway_elbv2.ModifyTargetGroup(ctx, input, gw.NATSConn, accountID)
	}),
	"ModifyTargetGroupAttributes": elbv2Handler(func(ctx context.Context, input *elbv2.ModifyTargetGroupAttributesInput, gw *GatewayConfig, accountID string) (any, error) {
		return gateway_elbv2.ModifyTargetGroupAttributes(ctx, input, gw.NATSConn, accountID)
	}),
	"DescribeTargetGroupAttributes": elbv2Handler(func(ctx context.Context, input *elbv2.DescribeTargetGroupAttributesInput, gw *GatewayConfig, accountID string) (any, error) {
		return gateway_elbv2.DescribeTargetGroupAttributes(ctx, input, gw.NATSConn, accountID)
	}),
	"ModifyLoadBalancerAttributes": elbv2Handler(func(ctx context.Context, input *elbv2.ModifyLoadBalancerAttributesInput, gw *GatewayConfig, accountID string) (any, error) {
		return gateway_elbv2.ModifyLoadBalancerAttributes(ctx, input, gw.NATSConn, accountID)
	}),
	"DescribeLoadBalancerAttributes": elbv2Handler(func(ctx context.Context, input *elbv2.DescribeLoadBalancerAttributesInput, gw *GatewayConfig, accountID string) (any, error) {
		return gateway_elbv2.DescribeLoadBalancerAttributes(ctx, input, gw.NATSConn, accountID)
	}),
	"SetSecurityGroups": elbv2Handler(func(ctx context.Context, input *elbv2.SetSecurityGroupsInput, gw *GatewayConfig, accountID string) (any, error) {
		return gateway_elbv2.SetSecurityGroups(ctx, input, gw.NATSConn, accountID)
	}),
	"SetIpAddressType": elbv2Handler(func(ctx context.Context, input *elbv2.SetIpAddressTypeInput, gw *GatewayConfig, accountID string) (any, error) {
		return gateway_elbv2.SetIpAddressType(ctx, input, gw.NATSConn, accountID)
	}),
	"SetSubnets": elbv2Handler(func(ctx context.Context, input *elbv2.SetSubnetsInput, gw *GatewayConfig, accountID string) (any, error) {
		return gateway_elbv2.SetSubnets(ctx, input, gw.NATSConn, accountID)
	}),
	"AddListenerCertificates": elbv2Handler(func(ctx context.Context, input *elbv2.AddListenerCertificatesInput, gw *GatewayConfig, accountID string) (any, error) {
		return gateway_elbv2.AddListenerCertificates(ctx, input, gw.NATSConn, accountID)
	}),
	"RemoveListenerCertificates": elbv2Handler(func(ctx context.Context, input *elbv2.RemoveListenerCertificatesInput, gw *GatewayConfig, accountID string) (any, error) {
		return gateway_elbv2.RemoveListenerCertificates(ctx, input, gw.NATSConn, accountID)
	}),
	"DescribeListenerCertificates": elbv2Handler(func(ctx context.Context, input *elbv2.DescribeListenerCertificatesInput, gw *GatewayConfig, accountID string) (any, error) {
		return gateway_elbv2.DescribeListenerCertificates(ctx, input, gw.NATSConn, accountID)
	}),
	"DescribeSSLPolicies": elbv2Handler(func(ctx context.Context, input *elbv2.DescribeSSLPoliciesInput, gw *GatewayConfig, accountID string) (any, error) {
		return gateway_elbv2.DescribeSSLPolicies(ctx, input, gw.NATSConn, accountID)
	}),
	"DescribeListenerAttributes": elbv2Handler(func(ctx context.Context, input *gateway_elbv2.DescribeListenerAttributesInput, gw *GatewayConfig, accountID string) (any, error) {
		return gateway_elbv2.DescribeListenerAttributes(input, accountID)
	}),
	"ModifyListenerAttributes": elbv2Handler(func(ctx context.Context, input *gateway_elbv2.ModifyListenerAttributesInput, gw *GatewayConfig, accountID string) (any, error) {
		return gateway_elbv2.ModifyListenerAttributes(input, accountID)
	}),
}

func (gw *GatewayConfig) ELBv2_Request(w http.ResponseWriter, r *http.Request) error {
	queryArgs, err := readQueryArgs(r)
	if err != nil {
		slog.DebugContext(r.Context(), "ELBv2: malformed query string", "err", err)
		return errors.New(awserrors.ErrorMalformedQueryString)
	}

	action := queryArgs["Action"]
	if action == "" {
		return errors.New(awserrors.ErrorMissingAction)
	}
	handler, ok := elbv2Actions[action]
	if !ok {
		return errors.New(awserrors.ErrorInvalidAction)
	}

	if err := gw.checkPolicy(r, "elasticloadbalancing", action); err != nil {
		return err
	}

	if gw.NATSConn == nil {
		return errors.New(awserrors.ErrorServerInternal)
	}

	accountID, _ := r.Context().Value(ctxAccountID).(string)
	if accountID == "" {
		slog.ErrorContext(r.Context(), "ELBv2_Request: no account ID in auth context")
		return errors.New(awserrors.ErrorServerInternal)
	}

	xmlOutput, err := handler(r.Context(), action, queryArgs, gw, accountID)
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
