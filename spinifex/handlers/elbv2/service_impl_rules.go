package handlers_elbv2

import (
	"errors"
	"fmt"
	"log/slog"
	"net/netip"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/elbv2"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	"github.com/mulgadc/spinifex/spinifex/utils"
)

// Regex guards for rule condition values — rejected at write time so the renderer
// can interpolate them safely. The renderer still validates defensively.
var (
	ruleHostPathRegex    = regexp.MustCompile(`^[A-Za-z0-9*?._/-]+$`)
	ruleHeaderValueRegex = regexp.MustCompile(`^[\x20-\x7E]+$`) // printable ASCII, no control bytes
	ruleHeaderNameRegex  = regexp.MustCompile(`^[A-Za-z0-9-]+$`)
	ruleMethodRegex      = regexp.MustCompile(`^[A-Z]+$`)
	ruleQueryStringRegex = regexp.MustCompile(`^[\x20-\x7E]+$`)
)

var allowedHTTPMethods = map[string]struct{}{
	"GET": {}, "HEAD": {}, "POST": {}, "PUT": {}, "DELETE": {},
	"CONNECT": {}, "OPTIONS": {}, "TRACE": {}, "PATCH": {},
}

func (s *ELBv2ServiceImpl) CreateRule(input *elbv2.CreateRuleInput, accountID string) (*elbv2.CreateRuleOutput, error) {
	if input == nil || input.ListenerArn == nil || *input.ListenerArn == "" {
		return nil, errors.New(awserrors.ErrorMissingParameter)
	}
	if input.Priority == nil {
		return nil, errors.New(awserrors.ErrorMissingParameter)
	}

	listener, err := s.store.GetListenerByArn(*input.ListenerArn)
	if err != nil {
		slog.Error("CreateRule: failed to get listener", "arn", *input.ListenerArn, "err", err)
		return nil, errors.New(awserrors.ErrorServerInternal)
	}
	if listener == nil || listener.AccountID != accountID {
		return nil, errors.New(awserrors.ErrorELBv2ListenerNotFound)
	}

	priority := int(*input.Priority)
	if priority < RuleMinPriority || priority > RuleMaxPriority {
		return nil, errors.New(awserrors.ErrorELBv2InvalidRulePriority)
	}

	conditions, err := validateAndConvertConditions(input.Conditions)
	if err != nil {
		return nil, err
	}

	actions, err := s.validateAndConvertRuleActions(input.Actions, listener.Protocol)
	if err != nil {
		return nil, err
	}

	existing, err := s.store.ListRulesByListener(listener.ListenerArn)
	if err != nil {
		slog.Error("CreateRule: failed to list rules", "listenerArn", listener.ListenerArn, "err", err)
		return nil, errors.New(awserrors.ErrorServerInternal)
	}
	if len(existing) >= MaxRulesPerListener {
		return nil, errors.New(awserrors.ErrorELBv2TooManyRules)
	}
	for _, r := range existing {
		if r.Priority == priority {
			return nil, errors.New(awserrors.ErrorELBv2PriorityInUse)
		}
	}

	ruleID := utils.GenerateResourceID("rule")
	ruleArn, err := buildRuleArn(listener.ListenerArn, ruleID)
	if err != nil {
		slog.Error("CreateRule: failed to build ARN", "listenerArn", listener.ListenerArn, "err", err)
		return nil, errors.New(awserrors.ErrorServerInternal)
	}

	tags := tagsFromSDK(input.Tags)

	record := &RuleRecord{
		RuleArn:     ruleArn,
		RuleID:      ruleID,
		ListenerArn: listener.ListenerArn,
		Priority:    priority,
		Conditions:  conditions,
		Actions:     actions,
		AccountID:   accountID,
		CreatedAt:   time.Now().UTC(),
		Tags:        tags,
	}

	if err := s.store.PutRule(record); err != nil {
		slog.Error("CreateRule: failed to persist", "ruleId", ruleID, "err", err)
		return nil, errors.New(awserrors.ErrorServerInternal)
	}

	if err := s.reloadListenerLB(listener); err != nil {
		return nil, err
	}

	slog.Info("CreateRule completed", "ruleArn", ruleArn, "listenerArn", listener.ListenerArn, "priority", priority, "accountID", accountID)

	return &elbv2.CreateRuleOutput{
		Rules: []*elbv2.Rule{ruleRecordToSDK(record)},
	}, nil
}

func (s *ELBv2ServiceImpl) ModifyRule(input *elbv2.ModifyRuleInput, accountID string) (*elbv2.ModifyRuleOutput, error) {
	if input == nil || input.RuleArn == nil || *input.RuleArn == "" {
		return nil, errors.New(awserrors.ErrorMissingParameter)
	}

	rule, err := s.store.GetRuleByArn(*input.RuleArn)
	if err != nil {
		slog.Error("ModifyRule: failed to get rule", "arn", *input.RuleArn, "err", err)
		return nil, errors.New(awserrors.ErrorServerInternal)
	}
	if rule == nil || rule.AccountID != accountID {
		return nil, errors.New(awserrors.ErrorELBv2RuleNotFound)
	}

	listener, err := s.store.GetListenerByArn(rule.ListenerArn)
	if err != nil || listener == nil {
		slog.Error("ModifyRule: failed to get listener", "arn", rule.ListenerArn, "err", err)
		return nil, errors.New(awserrors.ErrorServerInternal)
	}

	updated := *rule
	if len(input.Conditions) > 0 {
		conditions, err := validateAndConvertConditions(input.Conditions)
		if err != nil {
			return nil, err
		}
		updated.Conditions = conditions
	}
	if len(input.Actions) > 0 {
		actions, err := s.validateAndConvertRuleActions(input.Actions, listener.Protocol)
		if err != nil {
			return nil, err
		}
		updated.Actions = actions
	}

	if err := s.store.PutRule(&updated); err != nil {
		slog.Error("ModifyRule: failed to persist", "ruleId", updated.RuleID, "err", err)
		return nil, errors.New(awserrors.ErrorServerInternal)
	}

	if err := s.reloadListenerLB(listener); err != nil {
		return nil, err
	}

	slog.Info("ModifyRule completed", "ruleArn", updated.RuleArn, "accountID", accountID)

	return &elbv2.ModifyRuleOutput{
		Rules: []*elbv2.Rule{ruleRecordToSDK(&updated)},
	}, nil
}

func (s *ELBv2ServiceImpl) DeleteRule(input *elbv2.DeleteRuleInput, accountID string) (*elbv2.DeleteRuleOutput, error) {
	if input == nil || input.RuleArn == nil || *input.RuleArn == "" {
		return nil, errors.New(awserrors.ErrorMissingParameter)
	}

	rule, err := s.store.GetRuleByArn(*input.RuleArn)
	if err != nil {
		slog.Error("DeleteRule: failed to get rule", "arn", *input.RuleArn, "err", err)
		return nil, errors.New(awserrors.ErrorServerInternal)
	}
	if rule == nil || rule.AccountID != accountID {
		// Idempotent: AWS ELBv2 delete returns success on an absent (or
		// not-owned) rule, so tofu destroy retries converge.
		return &elbv2.DeleteRuleOutput{}, nil
	}

	listener, err := s.store.GetListenerByArn(rule.ListenerArn)
	if err != nil {
		slog.Error("DeleteRule: failed to get listener for reload", "listenerArn", rule.ListenerArn, "err", err)
		return nil, errors.New(awserrors.ErrorServerInternal)
	}

	if err := s.store.DeleteRule(rule.RuleID); err != nil {
		slog.Error("DeleteRule: failed to delete", "ruleId", rule.RuleID, "err", err)
		return nil, errors.New(awserrors.ErrorServerInternal)
	}

	if listener != nil {
		if err := s.reloadListenerLB(listener); err != nil {
			return nil, err
		}
	}

	slog.Info("DeleteRule completed", "ruleArn", *input.RuleArn, "accountID", accountID)

	return &elbv2.DeleteRuleOutput{}, nil
}

func (s *ELBv2ServiceImpl) DescribeRules(input *elbv2.DescribeRulesInput, accountID string) (*elbv2.DescribeRulesOutput, error) {
	if input == nil {
		return nil, errors.New(awserrors.ErrorMissingParameter)
	}

	out := &elbv2.DescribeRulesOutput{}

	switch {
	case input.ListenerArn != nil && *input.ListenerArn != "":
		listener, err := s.store.GetListenerByArn(*input.ListenerArn)
		if err != nil {
			slog.Error("DescribeRules: failed to get listener", "arn", *input.ListenerArn, "err", err)
			return nil, errors.New(awserrors.ErrorServerInternal)
		}
		if listener == nil || listener.AccountID != accountID {
			return nil, errors.New(awserrors.ErrorELBv2ListenerNotFound)
		}
		rules, err := s.store.ListRulesByListener(listener.ListenerArn)
		if err != nil {
			slog.Error("DescribeRules: failed to list", "listenerArn", listener.ListenerArn, "err", err)
			return nil, errors.New(awserrors.ErrorServerInternal)
		}
		for _, r := range rules {
			out.Rules = append(out.Rules, ruleRecordToSDK(r))
		}
		// AWS synthesises a "default" rule from the listener's default actions.
		out.Rules = append(out.Rules, defaultRuleFromListener(listener))

	case len(input.RuleArns) > 0:
		for _, arnPtr := range input.RuleArns {
			if arnPtr == nil || *arnPtr == "" {
				continue
			}
			r, err := s.store.GetRuleByArn(*arnPtr)
			if err != nil {
				slog.Error("DescribeRules: failed to get rule", "arn", *arnPtr, "err", err)
				return nil, errors.New(awserrors.ErrorServerInternal)
			}
			if r == nil || r.AccountID != accountID {
				return nil, errors.New(awserrors.ErrorELBv2RuleNotFound)
			}
			out.Rules = append(out.Rules, ruleRecordToSDK(r))
		}

	default:
		return nil, errors.New(awserrors.ErrorMissingParameter)
	}

	return out, nil
}

func (s *ELBv2ServiceImpl) SetRulePriorities(input *elbv2.SetRulePrioritiesInput, accountID string) (*elbv2.SetRulePrioritiesOutput, error) {
	if input == nil || len(input.RulePriorities) == 0 {
		return nil, errors.New(awserrors.ErrorMissingParameter)
	}

	// Resolve all rules first so duplicate-priority checks run before any write.
	type pendingUpdate struct {
		rule        *RuleRecord
		newPriority int
	}
	pending := make([]pendingUpdate, 0, len(input.RulePriorities))
	listenerArn := ""

	for _, rp := range input.RulePriorities {
		if rp == nil || rp.RuleArn == nil || *rp.RuleArn == "" || rp.Priority == nil {
			return nil, errors.New(awserrors.ErrorMissingParameter)
		}
		newPriority := int(*rp.Priority)
		if newPriority < RuleMinPriority || newPriority > RuleMaxPriority {
			return nil, errors.New(awserrors.ErrorELBv2InvalidRulePriority)
		}
		r, err := s.store.GetRuleByArn(*rp.RuleArn)
		if err != nil {
			slog.Error("SetRulePriorities: failed to get rule", "arn", *rp.RuleArn, "err", err)
			return nil, errors.New(awserrors.ErrorServerInternal)
		}
		if r == nil || r.AccountID != accountID {
			return nil, errors.New(awserrors.ErrorELBv2RuleNotFound)
		}
		if listenerArn == "" {
			listenerArn = r.ListenerArn
		} else if r.ListenerArn != listenerArn {
			return nil, errors.New(awserrors.ErrorELBv2InvalidConfigurationRequest)
		}
		pending = append(pending, pendingUpdate{rule: r, newPriority: newPriority})
	}

	// Detect duplicate priorities within the requested set.
	seen := make(map[int]struct{}, len(pending))
	for _, p := range pending {
		if _, dup := seen[p.newPriority]; dup {
			return nil, errors.New(awserrors.ErrorELBv2PriorityInUse)
		}
		seen[p.newPriority] = struct{}{}
	}

	// Detect collisions with rules not being renumbered in this call.
	all, err := s.store.ListRulesByListener(listenerArn)
	if err != nil {
		slog.Error("SetRulePriorities: failed to list rules", "listenerArn", listenerArn, "err", err)
		return nil, errors.New(awserrors.ErrorServerInternal)
	}
	renumbering := make(map[string]struct{}, len(pending))
	for _, p := range pending {
		renumbering[p.rule.RuleID] = struct{}{}
	}
	for _, r := range all {
		if _, isPending := renumbering[r.RuleID]; isPending {
			continue
		}
		if _, claimed := seen[r.Priority]; claimed {
			return nil, errors.New(awserrors.ErrorELBv2PriorityInUse)
		}
	}

	updated := make([]*elbv2.Rule, 0, len(pending))
	for _, p := range pending {
		next := *p.rule
		next.Priority = p.newPriority
		if err := s.store.PutRule(&next); err != nil {
			slog.Error("SetRulePriorities: failed to persist", "ruleId", next.RuleID, "err", err)
			return nil, errors.New(awserrors.ErrorServerInternal)
		}
		updated = append(updated, ruleRecordToSDK(&next))
	}

	listener, err := s.store.GetListenerByArn(listenerArn)
	if err == nil && listener != nil {
		if err := s.reloadListenerLB(listener); err != nil {
			return nil, err
		}
	}

	slog.Info("SetRulePriorities completed", "count", len(pending), "listenerArn", listenerArn, "accountID", accountID)

	return &elbv2.SetRulePrioritiesOutput{Rules: updated}, nil
}

// reloadListenerLB resolves the load balancer that owns the listener and
// regenerates its HAProxy config.
func (s *ELBv2ServiceImpl) reloadListenerLB(listener *ListenerRecord) error {
	lb, err := s.store.GetLoadBalancerByArn(listener.LoadBalancerArn)
	if err != nil {
		slog.Error("rule reload: failed to get LB", "arn", listener.LoadBalancerArn, "err", err)
		return errors.New(awserrors.ErrorServerInternal)
	}
	if lb == nil {
		return nil
	}
	if err := s.updateStoredConfig(lb); err != nil {
		slog.Error("rule reload: failed to update config", "lbArn", lb.LoadBalancerArn, "err", err)
		return errors.New(awserrors.ErrorServerInternal)
	}
	return nil
}

// validateAndConvertConditions enforces the rule-condition contract and returns
// the persisted-shape slice.
func validateAndConvertConditions(in []*elbv2.RuleCondition) ([]RuleCondition, error) {
	if len(in) == 0 {
		return nil, errors.New(awserrors.ErrorMissingParameter)
	}
	if len(in) > MaxConditionsPerRule {
		return nil, errors.New(awserrors.ErrorInvalidParameterValue)
	}

	out := make([]RuleCondition, 0, len(in))
	for _, c := range in {
		if c == nil || c.Field == nil {
			return nil, errors.New(awserrors.ErrorMissingParameter)
		}
		converted, err := validateCondition(c)
		if err != nil {
			return nil, err
		}
		out = append(out, converted)
	}
	return out, nil
}

func validateCondition(c *elbv2.RuleCondition) (RuleCondition, error) {
	field := *c.Field
	out := RuleCondition{Field: field}

	switch field {
	case RuleFieldHostHeader, RuleFieldPathPattern:
		vals, err := extractStringValues(c)
		if err != nil {
			return out, err
		}
		for _, v := range vals {
			if !validRuleStringValue(v, ruleHostPathRegex) {
				return out, errors.New(awserrors.ErrorInvalidParameterValue)
			}
		}
		out.Values = vals

	case RuleFieldHTTPHeader:
		if c.HttpHeaderConfig == nil || c.HttpHeaderConfig.HttpHeaderName == nil {
			return out, errors.New(awserrors.ErrorMissingParameter)
		}
		name := *c.HttpHeaderConfig.HttpHeaderName
		if len(name) == 0 || len(name) > MaxHTTPHeaderNameLen || !ruleHeaderNameRegex.MatchString(name) {
			return out, errors.New(awserrors.ErrorInvalidParameterValue)
		}
		vals := stringPtrSlice(c.HttpHeaderConfig.Values)
		if err := validateValueCount(vals); err != nil {
			return out, err
		}
		for _, v := range vals {
			if !validRuleStringValue(v, ruleHeaderValueRegex) {
				return out, errors.New(awserrors.ErrorInvalidParameterValue)
			}
		}
		out.HTTPHeaderName = name
		out.Values = vals

	case RuleFieldHTTPRequestMethod:
		if c.HttpRequestMethodConfig == nil {
			return out, errors.New(awserrors.ErrorMissingParameter)
		}
		vals := stringPtrSlice(c.HttpRequestMethodConfig.Values)
		if err := validateValueCount(vals); err != nil {
			return out, err
		}
		for _, v := range vals {
			if !ruleMethodRegex.MatchString(v) || len(v) > MaxConditionValueLen {
				return out, errors.New(awserrors.ErrorInvalidParameterValue)
			}
			if _, ok := allowedHTTPMethods[v]; !ok {
				return out, errors.New(awserrors.ErrorInvalidParameterValue)
			}
		}
		out.Values = vals

	case RuleFieldQueryString:
		if c.QueryStringConfig == nil || len(c.QueryStringConfig.Values) == 0 {
			return out, errors.New(awserrors.ErrorMissingParameter)
		}
		if len(c.QueryStringConfig.Values) > MaxValuesPerCondition {
			return out, errors.New(awserrors.ErrorInvalidParameterValue)
		}
		kvs := make([]RuleQueryStringKV, 0, len(c.QueryStringConfig.Values))
		for _, kv := range c.QueryStringConfig.Values {
			if kv == nil || kv.Value == nil {
				return out, errors.New(awserrors.ErrorMissingParameter)
			}
			key := ""
			if kv.Key != nil {
				key = *kv.Key
			}
			val := *kv.Value
			if len(val) == 0 || len(val) > MaxConditionValueLen || !ruleQueryStringRegex.MatchString(val) {
				return out, errors.New(awserrors.ErrorInvalidParameterValue)
			}
			if key != "" && (len(key) > MaxConditionValueLen || !ruleQueryStringRegex.MatchString(key)) {
				return out, errors.New(awserrors.ErrorInvalidParameterValue)
			}
			kvs = append(kvs, RuleQueryStringKV{Key: key, Value: val})
		}
		out.QueryStringKVs = kvs

	case RuleFieldSourceIP:
		if c.SourceIpConfig == nil {
			return out, errors.New(awserrors.ErrorMissingParameter)
		}
		vals := stringPtrSlice(c.SourceIpConfig.Values)
		if err := validateValueCount(vals); err != nil {
			return out, err
		}
		for _, v := range vals {
			if _, err := netip.ParsePrefix(v); err != nil {
				return out, errors.New(awserrors.ErrorInvalidParameterValue)
			}
		}
		out.Values = vals

	default:
		return out, errors.New(awserrors.ErrorInvalidParameterValue)
	}

	return out, nil
}

// extractStringValues returns the canonical Values for host-header / path-pattern
// conditions, accepting both flat and nested SDK shapes.
func extractStringValues(c *elbv2.RuleCondition) ([]string, error) {
	var vals []string
	switch *c.Field {
	case RuleFieldHostHeader:
		if c.HostHeaderConfig != nil {
			vals = stringPtrSlice(c.HostHeaderConfig.Values)
		}
	case RuleFieldPathPattern:
		if c.PathPatternConfig != nil {
			vals = stringPtrSlice(c.PathPatternConfig.Values)
		}
	}
	if len(vals) == 0 {
		vals = stringPtrSlice(c.Values)
	}
	if err := validateValueCount(vals); err != nil {
		return nil, err
	}
	return vals, nil
}

func validateValueCount(vals []string) error {
	if len(vals) == 0 {
		return errors.New(awserrors.ErrorMissingParameter)
	}
	if len(vals) > MaxValuesPerCondition {
		return errors.New(awserrors.ErrorInvalidParameterValue)
	}
	return nil
}

func validRuleStringValue(v string, allowed *regexp.Regexp) bool {
	if len(v) == 0 || len(v) > MaxConditionValueLen {
		return false
	}
	return allowed.MatchString(v)
}

func stringPtrSlice(in []*string) []string {
	out := make([]string, 0, len(in))
	for _, p := range in {
		if p == nil {
			continue
		}
		out = append(out, *p)
	}
	return out
}

func (s *ELBv2ServiceImpl) validateAndConvertRuleActions(in []*elbv2.Action, listenerProto string) ([]ListenerAction, error) {
	if len(in) == 0 {
		return nil, errors.New(awserrors.ErrorMissingParameter)
	}
	if len(in) > MaxActionsPerRule {
		return nil, errors.New(awserrors.ErrorELBv2TooManyActions)
	}

	out := make([]ListenerAction, 0, len(in))
	for _, a := range in {
		if a == nil || a.Type == nil {
			return nil, errors.New(awserrors.ErrorMissingParameter)
		}
		action := listenerActionFromSDK(a)
		switch action.Type {
		case ActionTypeForward:
			if action.TargetGroupArn == "" {
				return nil, errors.New(awserrors.ErrorMissingParameter)
			}
			tg, err := s.store.GetTargetGroupByArn(action.TargetGroupArn)
			if err != nil {
				slog.Error("rule action: failed to get target group", "arn", action.TargetGroupArn, "err", err)
				return nil, errors.New(awserrors.ErrorServerInternal)
			}
			if tg == nil {
				return nil, errors.New(awserrors.ErrorELBv2TargetGroupNotFound)
			}
			if !isCompatibleProtocol(listenerProto, tg.Protocol) {
				return nil, errors.New(awserrors.ErrorELBv2IncompatibleProtocols)
			}
		case ActionTypeRedirect:
			if err := validateRedirectAction(action.Redirect); err != nil {
				return nil, err
			}
		case ActionTypeFixedResponse:
			if action.FixedResponse == nil {
				return nil, errors.New(awserrors.ErrorMissingParameter)
			}
		default:
			return nil, errors.New(awserrors.ErrorELBv2InvalidConfigurationRequest)
		}
		out = append(out, action)
	}
	return out, nil
}

// buildRuleArn rewrites a listener ARN to a listener-rule ARN by replacing "listener/"
// with "listener-rule/" and appending the rule short ID.
func buildRuleArn(listenerArn, ruleID string) (string, error) {
	const old = ":listener/"
	before, after, ok := strings.Cut(listenerArn, old)
	if !ok {
		return "", fmt.Errorf("malformed listener arn: %s", listenerArn)
	}
	return before + ":listener-rule/" + after + "/" + ruleID, nil
}

// ruleRecordToSDK converts a stored rule record to the AWS SDK shape.
func ruleRecordToSDK(r *RuleRecord) *elbv2.Rule {
	rule := &elbv2.Rule{
		RuleArn:  aws.String(r.RuleArn),
		Priority: aws.String(strconv.Itoa(r.Priority)),
	}
	for _, c := range r.Conditions {
		rule.Conditions = append(rule.Conditions, ruleConditionToSDK(c))
	}
	for _, a := range r.Actions {
		rule.Actions = append(rule.Actions, listenerActionToSDK(a))
	}
	return rule
}

func ruleConditionToSDK(c RuleCondition) *elbv2.RuleCondition {
	out := &elbv2.RuleCondition{Field: aws.String(c.Field)}

	switch c.Field {
	case RuleFieldHostHeader:
		out.HostHeaderConfig = &elbv2.HostHeaderConditionConfig{Values: aws.StringSlice(c.Values)}
		out.Values = aws.StringSlice(c.Values)
	case RuleFieldPathPattern:
		out.PathPatternConfig = &elbv2.PathPatternConditionConfig{Values: aws.StringSlice(c.Values)}
		out.Values = aws.StringSlice(c.Values)
	case RuleFieldHTTPHeader:
		out.HttpHeaderConfig = &elbv2.HttpHeaderConditionConfig{
			HttpHeaderName: aws.String(c.HTTPHeaderName),
			Values:         aws.StringSlice(c.Values),
		}
	case RuleFieldHTTPRequestMethod:
		out.HttpRequestMethodConfig = &elbv2.HttpRequestMethodConditionConfig{Values: aws.StringSlice(c.Values)}
	case RuleFieldQueryString:
		kvs := make([]*elbv2.QueryStringKeyValuePair, 0, len(c.QueryStringKVs))
		for _, kv := range c.QueryStringKVs {
			pair := &elbv2.QueryStringKeyValuePair{Value: aws.String(kv.Value)}
			if kv.Key != "" {
				pair.Key = aws.String(kv.Key)
			}
			kvs = append(kvs, pair)
		}
		out.QueryStringConfig = &elbv2.QueryStringConditionConfig{Values: kvs}
	case RuleFieldSourceIP:
		out.SourceIpConfig = &elbv2.SourceIpConditionConfig{Values: aws.StringSlice(c.Values)}
	}

	return out
}

// defaultRuleFromListener synthesises the IsDefault=true rule AWS returns
// alongside user rules in DescribeRules. The default rule is not stored — it
// derives from ListenerRecord.DefaultActions.
func defaultRuleFromListener(l *ListenerRecord) *elbv2.Rule {
	rule := &elbv2.Rule{
		Priority:  aws.String("default"),
		IsDefault: aws.Bool(true),
	}
	for _, a := range l.DefaultActions {
		rule.Actions = append(rule.Actions, listenerActionToSDK(a))
	}
	return rule
}
