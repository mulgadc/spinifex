package gateway_elbv2

import (
	"errors"
	"log/slog"
	"maps"
	"slices"
	"strings"

	"github.com/mulgadc/spinifex/spinifex/arn"
	"github.com/mulgadc/spinifex/spinifex/awsec2query"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
)

// The resource a policy check evaluates against when the request names nothing
// in particular, or when the identifier cannot be resolved at gate time.
const anyResource = "*"

// Stands in for the short id of a resource the request is about to create. It
// is a value, so a policy scoped to the parent path matches it where "*" would
// not; a Deny naming the stored id cannot fire, because it does not exist yet.
const anyID = "*"

// Where an action's resource is named, and how it becomes an ARN. The caller
// supplies a full ARN for everything but the four creates, which derive one
// from a parent ARN or from the name the request asks for.
type resourceSource uint8

const (
	sourceAny resourceSource = iota
	sourceLoadBalancerARN
	sourceTargetGroupARN
	sourceListenerARN
	sourceRuleARN
	sourceRulePriorityARNs
	sourceResourceARNs
	sourceNewLoadBalancer
	sourceNewTargetGroup
	sourceNewListener
	sourceNewRule
	sourceDefaultActionTargetGroups
	sourceActionTargetGroups
)

// Every action ELBv2_Request serves, with the resources AWS evaluates it
// against. Exhaustive by contract: a completeness test compares this table with
// the dispatch table in both directions, so an action cannot be added with a
// silent account-wide grant.
var elbv2Scopes = map[string][]resourceSource{
	// Load balancers.
	"CreateLoadBalancer":             {sourceNewLoadBalancer},
	"DeleteLoadBalancer":             {sourceLoadBalancerARN},
	"ModifyLoadBalancerAttributes":   {sourceLoadBalancerARN},
	"DescribeLoadBalancerAttributes": {sourceLoadBalancerARN},
	"SetSecurityGroups":              {sourceLoadBalancerARN},
	"SetSubnets":                     {sourceLoadBalancerARN},
	"SetIpAddressType":               {sourceLoadBalancerARN},

	// Target groups.
	"CreateTargetGroup":             {sourceNewTargetGroup},
	"DeleteTargetGroup":             {sourceTargetGroupARN},
	"ModifyTargetGroup":             {sourceTargetGroupARN},
	"ModifyTargetGroupAttributes":   {sourceTargetGroupARN},
	"DescribeTargetGroupAttributes": {sourceTargetGroupARN},
	"RegisterTargets":               {sourceTargetGroupARN},
	"DeregisterTargets":             {sourceTargetGroupARN},
	"DescribeTargetHealth":          {sourceTargetGroupARN},

	// Listeners. A create evaluates the load balancer, the listener it is about
	// to create and every target group its default actions forward to, matching
	// AWS; a redirect-only action names no target group and drops out.
	"CreateListener":               {sourceLoadBalancerARN, sourceNewListener, sourceDefaultActionTargetGroups},
	"ModifyListener":               {sourceListenerARN, sourceDefaultActionTargetGroups},
	"DeleteListener":               {sourceListenerARN},
	"AddListenerCertificates":      {sourceListenerARN},
	"RemoveListenerCertificates":   {sourceListenerARN},
	"DescribeListenerCertificates": {sourceListenerARN},
	"DescribeListenerAttributes":   {sourceListenerARN},
	"ModifyListenerAttributes":     {sourceListenerARN},

	// Rules.
	"CreateRule":        {sourceListenerARN, sourceNewRule, sourceActionTargetGroups},
	"ModifyRule":        {sourceRuleARN, sourceActionTargetGroups},
	"DeleteRule":        {sourceRuleARN},
	"SetRulePriorities": {sourceRulePriorityARNs},

	// Tags, which name their resources by ARN whatever type they are.
	"AddTags":      {sourceResourceARNs},
	"RemoveTags":   {sourceResourceARNs},
	"DescribeTags": {sourceResourceARNs},

	// Describes. "*" is fidelity, not a stub: AWS documents no resource type for
	// these five either, so a pasted AWS policy behaves the same way here.
	"DescribeLoadBalancers": {sourceAny},
	"DescribeTargetGroups":  {sourceAny},
	"DescribeListeners":     {sourceAny},
	"DescribeRules":         {sourceAny},
	"DescribeSSLPolicies":   {sourceAny},

	// The lb-agent routes carry only the load balancer's short id. Recovering
	// its name and type to build an ARN needs a NATS round trip on every
	// heartbeat, so both authorize account-wide.
	"LBAgentHeartbeat": {sourceAny},
	"GetLBConfig":      {sourceAny},
}

// HasScope reports whether action has an explicit ELBv2 scope-table entry.
func HasScope(action string) bool {
	_, ok := elbv2Scopes[action]
	return ok
}

// ScopedActions returns every action represented in the ELBv2 scope table, so a
// scope left behind by a deleted or renamed action fails completeness too.
func ScopedActions() []string {
	return slices.Sorted(maps.Keys(elbv2Scopes))
}

// ResourceARNs resolves the resources an ELBv2 request authorizes against from
// the same parsed SDK input the handler receives. A caller-supplied ARN naming
// another partition, region or account is rejected here rather than authorized
// verbatim, because that is how the gate and the handler come to disagree about
// which object is being addressed.
func ResourceARNs(action, region, accountID string, input any) ([]string, error) {
	sources, ok := elbv2Scopes[action]
	if !ok {
		slog.Error("ELBv2 authz: action is served but absent from the scope table", "action", action)
		return nil, errors.New(awserrors.ErrorInvalidAction)
	}

	resources := make([]string, 0, len(sources))
	seen := make(map[string]struct{}, len(sources))
	for _, source := range sources {
		resolved, err := resolve(source, action, region, accountID, input)
		if err != nil {
			return nil, err
		}
		for _, resource := range resolved {
			// An unresolved member drops out rather than contributing "*", which no
			// scoped Allow can match and which would deny a call AWS permits.
			if resource == anyResource {
				continue
			}
			if _, duplicate := seen[resource]; duplicate {
				continue
			}
			if len(resources) >= awsec2query.MaxSliceLen {
				return nil, errors.New(awserrors.ErrorMalformedQueryString)
			}
			seen[resource] = struct{}{}
			resources = append(resources, resource)
		}
	}
	if len(resources) == 0 {
		return []string{anyResource}, nil
	}
	return resources, nil
}

func resolve(source resourceSource, action, region, accountID string, input any) ([]string, error) {
	switch source {
	case sourceAny:
		return []string{anyResource}, nil

	case sourceLoadBalancerARN:
		return suppliedARNs(region, accountID, input, "LoadBalancerArn")

	case sourceTargetGroupARN:
		return suppliedARNs(region, accountID, input, "TargetGroupArn")

	case sourceListenerARN:
		return suppliedARNs(region, accountID, input, "ListenerArn")

	case sourceRuleARN:
		return suppliedARNs(region, accountID, input, "RuleArn")

	case sourceRulePriorityARNs:
		return suppliedARNs(region, accountID, input, "RulePriorities.RuleArn")

	case sourceResourceARNs:
		return suppliedARNs(region, accountID, input, "ResourceArns")

	case sourceNewLoadBalancer:
		name := firstValue(input, "Name")
		if name == "" {
			return []string{anyResource}, nil
		}
		return []string{arn.FormatELBv2LoadBalancer(region, accountID, name, anyID, firstValue(input, "Type"))}, nil

	case sourceNewTargetGroup:
		name := firstValue(input, "Name")
		if name == "" {
			return []string{anyResource}, nil
		}
		return []string{arn.FormatELBv2TargetGroup(region, accountID, name, anyID)}, nil

	case sourceNewListener:
		return derivedChild(region, accountID, input, "LoadBalancerArn", arn.ELBv2LoadBalancer, arn.ELBv2Listener)

	case sourceNewRule:
		return derivedChild(region, accountID, input, "ListenerArn", arn.ELBv2Listener, arn.ELBv2ListenerRule)

	case sourceDefaultActionTargetGroups:
		return suppliedARNs(region, accountID, input,
			"DefaultActions.TargetGroupArn", "DefaultActions.ForwardConfig.TargetGroups.TargetGroupArn")

	case sourceActionTargetGroups:
		return suppliedARNs(region, accountID, input,
			"Actions.TargetGroupArn", "Actions.ForwardConfig.TargetGroups.TargetGroupArn")

	default:
		slog.Error("ELBv2 authz: unhandled resource source, failing closed", "action", action, "source", source)
		return nil, errors.New(awserrors.ErrorInternalError)
	}
}

// suppliedARNs validates every ARN the request names at the given field paths.
// An absent identifier authorizes account-wide, so a malformed request stays
// the handler's validation fault rather than becoming an authorization failure.
func suppliedARNs(region, accountID string, input any, paths ...string) ([]string, error) {
	var resources []string
	for _, path := range paths {
		for _, value := range awsec2query.StringValuesAt(input, path) {
			if value == "" {
				continue
			}
			// A nested list can fan out past what any single list may hold, so the
			// bound is asserted here rather than inherited from the parser.
			if len(resources) >= awsec2query.MaxSliceLen {
				return nil, errors.New(awserrors.ErrorMalformedQueryString)
			}
			if err := checkARN(value, region, accountID); err != nil {
				return nil, err
			}
			resources = append(resources, value)
		}
	}
	if len(resources) == 0 {
		return []string{anyResource}, nil
	}
	return resources, nil
}

// derivedChild builds the ARN of a resource the request is about to create
// under the parent it names. The parent already carries the load balancer's
// name, type and id in the spelling the handler will reuse, so the child is
// retyped from it rather than rebuilt from components the gate cannot see.
func derivedChild(region, accountID string, input any, path string, parent, child arn.ELBv2ResourceType) ([]string, error) {
	value := firstValue(input, path)
	if value == "" {
		return []string{anyResource}, nil
	}
	if err := checkARN(value, region, accountID); err != nil {
		return nil, err
	}
	parsed, _ := arn.ParseELBv2(value)
	if parsed.Kind != parent || parsed.Resource == "" {
		return nil, arnError(value, "expected an ELBv2 "+string(parent)+" ARN")
	}
	return []string{arn.FormatELBv2Resource(region, accountID, child, parsed.Resource+"/"+anyID)}, nil
}

// checkARN requires a caller-supplied ARN to be an ELBv2 ARN in this endpoint's
// region and the caller's own account.
func checkARN(value, region, accountID string) error {
	parsed, ok := arn.ParseELBv2(value)
	if !ok {
		return arnError(value, "expected the form arn:aws:elasticloadbalancing:{region}:{account}:{type}/{path}")
	}
	if parsed.Region != region {
		return arnError(value, "region "+parsed.Region+" does not match this endpoint's region "+region)
	}
	if parsed.AccountID != accountID {
		return arnError(value, "the resource belongs to another account")
	}
	if strings.TrimSpace(parsed.Resource) == "" {
		return arnError(value, "the resource path is empty")
	}
	return nil
}

func arnError(value, why string) error {
	return awserrors.Errorf(awserrors.ErrorInvalidParameterValue, "%q is not a valid ELBv2 ARN: %s", value, why)
}

func firstValue(input any, path string) string {
	for _, value := range awsec2query.StringValuesAt(input, path) {
		if value != "" {
			return value
		}
	}
	return ""
}
