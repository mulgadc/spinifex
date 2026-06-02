package handlers_elbv2

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"

	"github.com/mulgadc/spinifex/spinifex/migrate"
	"github.com/mulgadc/spinifex/spinifex/utils"
	"github.com/nats-io/nats.go"
)

const (
	KVBucketELBv2        = "spinifex-elbv2"
	KVBucketELBv2Version = 1

	// Key prefixes for different resource types within the single bucket
	KeyPrefixLB       = "lb."
	KeyPrefixTG       = "tg."
	KeyPrefixListener = "listener."
	KeyPrefixRule     = "rule."
)

// Store provides CRUD operations for ELBv2 resources backed by JetStream KV.
type Store struct {
	kv nats.KeyValue
}

// NewStore creates a new ELBv2 store using the provided NATS connection.
func NewStore(nc *nats.Conn) (*Store, error) {
	js, err := nc.JetStream()
	if err != nil {
		return nil, fmt.Errorf("failed to get JetStream context: %w", err)
	}

	kv, err := utils.GetOrCreateKVBucket(js, KVBucketELBv2, 1)
	if err != nil {
		return nil, fmt.Errorf("failed to create KV bucket %s: %w", KVBucketELBv2, err)
	}

	if err := migrate.DefaultRegistry.RunKV(KVBucketELBv2, kv, KVBucketELBv2Version); err != nil {
		return nil, fmt.Errorf("migrate %s: %w", KVBucketELBv2, err)
	}

	slog.Info("ELBv2 store initialized", "bucket", KVBucketELBv2)
	return &Store{kv: kv}, nil
}

// --- Load Balancer CRUD ---

// PutLoadBalancer stores a load balancer record.
func (s *Store) PutLoadBalancer(lb *LoadBalancerRecord) error {
	data, err := json.Marshal(lb)
	if err != nil {
		return fmt.Errorf("marshal load balancer: %w", err)
	}
	_, err = s.kv.Put(KeyPrefixLB+lb.LoadBalancerID, data)
	return err
}

// GetLoadBalancer retrieves a load balancer by its short ID.
func (s *Store) GetLoadBalancer(lbID string) (*LoadBalancerRecord, error) {
	entry, err := s.kv.Get(KeyPrefixLB + lbID)
	if err != nil {
		if errors.Is(err, nats.ErrKeyNotFound) {
			return nil, nil
		}
		return nil, err
	}
	var lb LoadBalancerRecord
	if err := json.Unmarshal(entry.Value(), &lb); err != nil {
		return nil, fmt.Errorf("unmarshal load balancer: %w", err)
	}
	return &lb, nil
}

// DeleteLoadBalancer removes a load balancer by its short ID.
func (s *Store) DeleteLoadBalancer(lbID string) error {
	err := s.kv.Delete(KeyPrefixLB + lbID)
	if err != nil && !errors.Is(err, nats.ErrKeyNotFound) {
		return err
	}
	return nil
}

// ListLoadBalancers returns all load balancer records.
func (s *Store) ListLoadBalancers() ([]*LoadBalancerRecord, error) {
	return listByPrefix[LoadBalancerRecord](s.kv, KeyPrefixLB)
}

// GetLoadBalancerByArn finds a load balancer by its ARN via a direct KV lookup
// on the short ID embedded in the ARN's final path segment. Falls back to a
// linear scan only if the ARN can't be parsed. Terraform hits Describe*Attributes
// on every plan/refresh so this must be O(1), not O(n).
func (s *Store) GetLoadBalancerByArn(arn string) (*LoadBalancerRecord, error) {
	// ELBv2 LB ARN: arn:aws:elasticloadbalancing:{region}:{account}:loadbalancer/{app,net}/{name}/{lbID}
	idx := strings.LastIndex(arn, "/")
	if idx < 0 || idx == len(arn)-1 {
		return nil, nil
	}
	lbID := arn[idx+1:]
	lb, err := s.GetLoadBalancer(lbID)
	if err != nil {
		return nil, err
	}
	// Defence-in-depth: ensure the record actually belongs to this ARN.
	// Short IDs are random hex, so collisions are effectively impossible, but
	// a mismatch here would indicate KV corruption and must not be silently
	// served as a successful lookup.
	if lb == nil || lb.LoadBalancerArn != arn {
		return nil, nil
	}
	return lb, nil
}

// GetLoadBalancerByName finds a load balancer by name within an account.
func (s *Store) GetLoadBalancerByName(name, accountID string) (*LoadBalancerRecord, error) {
	lbs, err := s.ListLoadBalancers()
	if err != nil {
		return nil, err
	}
	for _, lb := range lbs {
		if lb.Name == name && lb.AccountID == accountID {
			return lb, nil
		}
	}
	return nil, nil
}

// --- Target Group CRUD ---

// PutTargetGroup stores a target group record.
func (s *Store) PutTargetGroup(tg *TargetGroupRecord) error {
	data, err := json.Marshal(tg)
	if err != nil {
		return fmt.Errorf("marshal target group: %w", err)
	}
	_, err = s.kv.Put(KeyPrefixTG+tg.TargetGroupID, data)
	return err
}

// GetTargetGroup retrieves a target group by its short ID.
func (s *Store) GetTargetGroup(tgID string) (*TargetGroupRecord, error) {
	entry, err := s.kv.Get(KeyPrefixTG + tgID)
	if err != nil {
		if errors.Is(err, nats.ErrKeyNotFound) {
			return nil, nil
		}
		return nil, err
	}
	var tg TargetGroupRecord
	if err := json.Unmarshal(entry.Value(), &tg); err != nil {
		return nil, fmt.Errorf("unmarshal target group: %w", err)
	}
	return &tg, nil
}

// DeleteTargetGroup removes a target group by its short ID.
func (s *Store) DeleteTargetGroup(tgID string) error {
	err := s.kv.Delete(KeyPrefixTG + tgID)
	if err != nil && !errors.Is(err, nats.ErrKeyNotFound) {
		return err
	}
	return nil
}

// ListTargetGroups returns all target group records.
func (s *Store) ListTargetGroups() ([]*TargetGroupRecord, error) {
	return listByPrefix[TargetGroupRecord](s.kv, KeyPrefixTG)
}

// GetTargetGroupByArn finds a target group by its ARN via a direct KV lookup
// on the short ID embedded in the ARN's final path segment. See
// GetLoadBalancerByArn for the motivation — Terraform's per-plan
// DescribeTargetGroupAttributes storm must be O(1).
func (s *Store) GetTargetGroupByArn(arn string) (*TargetGroupRecord, error) {
	// ELBv2 TG ARN: arn:aws:elasticloadbalancing:{region}:{account}:targetgroup/{name}/{tgID}
	idx := strings.LastIndex(arn, "/")
	if idx < 0 || idx == len(arn)-1 {
		return nil, nil
	}
	tgID := arn[idx+1:]
	tg, err := s.GetTargetGroup(tgID)
	if err != nil {
		return nil, err
	}
	if tg == nil || tg.TargetGroupArn != arn {
		return nil, nil
	}
	return tg, nil
}

// TargetGroupsForLB returns only the target groups attached to a load balancer
// via its listeners. It follows LB ID → LB ARN → listeners → TG ARNs → TGs.
func (s *Store) TargetGroupsForLB(lbID string) ([]*TargetGroupRecord, error) {
	lb, err := s.GetLoadBalancer(lbID)
	if err != nil {
		return nil, fmt.Errorf("get load balancer %s: %w", lbID, err)
	}
	if lb == nil {
		return nil, nil
	}

	listeners, err := s.ListListenersByLB(lb.LoadBalancerArn)
	if err != nil {
		return nil, fmt.Errorf("list listeners for %s: %w", lbID, err)
	}

	// Collect unique TG IDs from listener default actions and rule actions.
	// Rule TGs must be included so the health checker can update their state
	// when HAProxy reports server status — HAProxy probes every backend it
	// renders, including rule backends.
	seen := make(map[string]struct{})
	collect := func(tgArn string) {
		if tgArn == "" {
			return
		}
		// TG ARN format: arn:aws:elasticloadbalancing:{region}:{account}:targetgroup/{name}/{tgID}
		if idx := strings.LastIndex(tgArn, "/"); idx >= 0 {
			seen[tgArn[idx+1:]] = struct{}{}
		}
	}
	for _, l := range listeners {
		for _, a := range l.DefaultActions {
			collect(a.TargetGroupArn)
		}
		rules, err := s.ListRulesByListener(l.ListenerArn)
		if err != nil {
			return nil, fmt.Errorf("list rules for listener %s: %w", l.ListenerArn, err)
		}
		for _, r := range rules {
			for _, a := range r.Actions {
				collect(a.TargetGroupArn)
			}
		}
	}

	tgs := make([]*TargetGroupRecord, 0, len(seen))
	for tgID := range seen {
		tg, err := s.GetTargetGroup(tgID)
		if err != nil {
			return nil, fmt.Errorf("get target group %s: %w", tgID, err)
		}
		if tg != nil {
			tgs = append(tgs, tg)
		}
	}
	return tgs, nil
}

// GetTargetGroupByName finds a target group by name within a VPC.
func (s *Store) GetTargetGroupByName(name, vpcID string) (*TargetGroupRecord, error) {
	tgs, err := s.ListTargetGroups()
	if err != nil {
		return nil, err
	}
	for _, tg := range tgs {
		if tg.Name == name && tg.VpcId == vpcID {
			return tg, nil
		}
	}
	return nil, nil
}

// --- Listener CRUD ---

// PutListener stores a listener record.
func (s *Store) PutListener(l *ListenerRecord) error {
	data, err := json.Marshal(l)
	if err != nil {
		return fmt.Errorf("marshal listener: %w", err)
	}
	_, err = s.kv.Put(KeyPrefixListener+l.ListenerID, data)
	return err
}

// GetListener retrieves a listener by its short ID.
func (s *Store) GetListener(listenerID string) (*ListenerRecord, error) {
	entry, err := s.kv.Get(KeyPrefixListener + listenerID)
	if err != nil {
		if errors.Is(err, nats.ErrKeyNotFound) {
			return nil, nil
		}
		return nil, err
	}
	var l ListenerRecord
	if err := json.Unmarshal(entry.Value(), &l); err != nil {
		return nil, fmt.Errorf("unmarshal listener: %w", err)
	}
	return &l, nil
}

// DeleteListener removes a listener by its short ID.
func (s *Store) DeleteListener(listenerID string) error {
	err := s.kv.Delete(KeyPrefixListener + listenerID)
	if err != nil && !errors.Is(err, nats.ErrKeyNotFound) {
		return err
	}
	return nil
}

// ListListeners returns all listener records.
func (s *Store) ListListeners() ([]*ListenerRecord, error) {
	return listByPrefix[ListenerRecord](s.kv, KeyPrefixListener)
}

// ListListenersByLB returns all listeners for a specific load balancer ARN.
func (s *Store) ListListenersByLB(lbArn string) ([]*ListenerRecord, error) {
	all, err := s.ListListeners()
	if err != nil {
		return nil, err
	}
	var result []*ListenerRecord
	for _, l := range all {
		if l.LoadBalancerArn == lbArn {
			result = append(result, l)
		}
	}
	return result, nil
}

// GetListenerByArn finds a listener by its ARN.
func (s *Store) GetListenerByArn(arn string) (*ListenerRecord, error) {
	listeners, err := s.ListListeners()
	if err != nil {
		return nil, err
	}
	for _, l := range listeners {
		if l.ListenerArn == arn {
			return l, nil
		}
	}
	return nil, nil
}

// --- Rule CRUD ---

// PutRule stores a rule record.
func (s *Store) PutRule(r *RuleRecord) error {
	data, err := json.Marshal(r)
	if err != nil {
		return fmt.Errorf("marshal rule: %w", err)
	}
	_, err = s.kv.Put(KeyPrefixRule+r.RuleID, data)
	return err
}

// GetRule retrieves a rule by its short ID.
func (s *Store) GetRule(ruleID string) (*RuleRecord, error) {
	entry, err := s.kv.Get(KeyPrefixRule + ruleID)
	if err != nil {
		if errors.Is(err, nats.ErrKeyNotFound) {
			return nil, nil
		}
		return nil, err
	}
	var r RuleRecord
	if err := json.Unmarshal(entry.Value(), &r); err != nil {
		return nil, fmt.Errorf("unmarshal rule: %w", err)
	}
	return &r, nil
}

// DeleteRule removes a rule by its short ID.
func (s *Store) DeleteRule(ruleID string) error {
	err := s.kv.Delete(KeyPrefixRule + ruleID)
	if err != nil && !errors.Is(err, nats.ErrKeyNotFound) {
		return err
	}
	return nil
}

// ListRules returns all rule records.
func (s *Store) ListRules() ([]*RuleRecord, error) {
	return listByPrefix[RuleRecord](s.kv, KeyPrefixRule)
}

// ListRulesByListener returns all rules attached to a listener ARN, sorted by
// ascending priority. Callers downstream of this method (HAProxy renderer,
// SetRulePriorities) rely on the sort.
func (s *Store) ListRulesByListener(listenerArn string) ([]*RuleRecord, error) {
	all, err := s.ListRules()
	if err != nil {
		return nil, err
	}
	var result []*RuleRecord
	for _, r := range all {
		if r.ListenerArn == listenerArn {
			result = append(result, r)
		}
	}
	sort.SliceStable(result, func(i, j int) bool { return result[i].Priority < result[j].Priority })
	return result, nil
}

// GetRuleByArn finds a rule by its ARN. Falls back to linear scan because the
// listener-rule ARN structure embeds the rule short ID after several
// listener-specific segments; parsing it is brittle and rules-per-account is
// bounded.
func (s *Store) GetRuleByArn(arn string) (*RuleRecord, error) {
	rules, err := s.ListRules()
	if err != nil {
		return nil, err
	}
	for _, r := range rules {
		if r.RuleArn == arn {
			return r, nil
		}
	}
	return nil, nil
}

// --- Generic helpers ---

// listByPrefix returns all records with keys matching the given prefix.
func listByPrefix[T any](kv nats.KeyValue, prefix string) ([]*T, error) {
	keys, err := kv.Keys()
	if err != nil {
		if errors.Is(err, nats.ErrNoKeysFound) {
			return nil, nil
		}
		return nil, err
	}

	var result []*T
	for _, key := range keys {
		if key == utils.VersionKey {
			continue
		}
		if !strings.HasPrefix(key, prefix) {
			continue
		}

		entry, err := kv.Get(key)
		if err != nil {
			if errors.Is(err, nats.ErrKeyNotFound) {
				continue
			}
			return nil, err
		}

		var record T
		if err := json.Unmarshal(entry.Value(), &record); err != nil {
			slog.Error("Failed to unmarshal ELBv2 record", "key", key, "err", err)
			continue
		}

		result = append(result, &record)
	}

	return result, nil
}
