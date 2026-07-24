package handlers_elbv2

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"

	"github.com/mulgadc/spinifex/spinifex/kvutil"
	"github.com/mulgadc/spinifex/spinifex/migrate"
	"github.com/mulgadc/spinifex/spinifex/utils"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
)

const (
	KVBucketELBv2        = "spinifex-elbv2"
	KVBucketELBv2Version = 1

	// Key prefixes for different resource types within the single bucket.
	KeyPrefixLB       = "lb."
	KeyPrefixTG       = "tg."
	KeyPrefixListener = "listener."
	KeyPrefixRule     = "rule."
	// KeyPrefixLBName is the per-account LB-name claim key that makes the LB
	// name an atomically claimable identity, preventing concurrent same-name
	// creates from both launching a VM.
	KeyPrefixLBName = "lbname."

	// maxLBNameClaimRetries bounds the crash-orphan CAS-reclaim loop in ClaimLBName.
	maxLBNameClaimRetries = 5
)

// Store provides CRUD operations for ELBv2 resources backed by JetStream KV.
// Every method takes the caller's context as its leading parameter, so a read or
// write honors the caller's deadline and cancellation instead of the fixed
// internal wait the legacy KV API applied.
type Store struct {
	kv jetstream.KeyValue
}

// NewStore creates a new ELBv2 store using the provided NATS connection. The
// context bounds bucket creation and the schema migration only; per-operation
// contexts come from the Store methods.
func NewStore(ctx context.Context, nc *nats.Conn) (*Store, error) {
	js, err := jetstream.New(nc)
	if err != nil {
		return nil, fmt.Errorf("failed to get JetStream context: %w", err)
	}

	kv, err := kvutil.GetOrCreateBucket(ctx, js, KVBucketELBv2, 1)
	if err != nil {
		return nil, fmt.Errorf("failed to create KV bucket %s: %w", KVBucketELBv2, err)
	}

	if err := migrate.DefaultRegistry.RunKV(ctx, KVBucketELBv2, kv, KVBucketELBv2Version); err != nil {
		return nil, fmt.Errorf("migrate %s: %w", KVBucketELBv2, err)
	}

	slog.Info("ELBv2 store initialized", "bucket", KVBucketELBv2)
	return &Store{kv: kv}, nil
}

// --- Load Balancer CRUD ---

// LBNameKey returns the per-account name-claim key for an LB name.
func LBNameKey(name, accountID string) string {
	return KeyPrefixLBName + accountID + "." + name
}

// ClaimLBName atomically claims the LB name; ok=true means this caller owns it,
// dup=true means a live LB already holds it. An orphaned claim (owner resolves to
// no record) is reclaimed via CAS. Idempotency barrier for CreateLoadBalancer.
func (s *Store) ClaimLBName(ctx context.Context, name, accountID, lbID string) (ok bool, dup bool, err error) {
	key := LBNameKey(name, accountID)
	for range maxLBNameClaimRetries {
		if _, cerr := s.kv.Create(ctx, key, []byte(lbID)); cerr == nil {
			return true, false, nil
		} else if !errors.Is(cerr, jetstream.ErrKeyExists) {
			return false, false, fmt.Errorf("kv create %s: %w", key, cerr)
		}
		entry, gerr := s.kv.Get(ctx, key)
		if gerr != nil {
			if errors.Is(gerr, jetstream.ErrKeyNotFound) {
				continue // raced vanish; retry the create
			}
			return false, false, fmt.Errorf("kv get %s: %w", key, gerr)
		}
		ownerID := string(entry.Value())
		if ownerID != "" && ownerID != lbID {
			rec, rerr := s.GetLoadBalancer(ctx, ownerID)
			if rerr != nil {
				return false, false, fmt.Errorf("resolve LB name owner %s: %w", ownerID, rerr)
			}
			if rec != nil {
				return false, true, nil // live LB holds the name
			}
		}
		// Orphaned (crashed prior create) or already ours: CAS-take the claim.
		if _, uerr := s.kv.Update(ctx, key, []byte(lbID), entry.Revision()); uerr != nil {
			if errors.Is(uerr, jetstream.ErrKeyExists) {
				continue // lost the CAS race; re-read
			}
			return false, false, fmt.Errorf("kv update %s: %w", key, uerr)
		}
		return true, false, nil
	}
	return false, false, fmt.Errorf("elbv2: claim LB name %s exhausted retries", name)
}

// ReleaseLBName deletes the name claim. A missing key is success (idempotent),
// so create-rollback paths and DeleteLoadBalancer can call it unconditionally.
func (s *Store) ReleaseLBName(ctx context.Context, name, accountID string) error {
	key := LBNameKey(name, accountID)
	if err := s.kv.Delete(ctx, key); err != nil && !errors.Is(err, jetstream.ErrKeyNotFound) {
		return fmt.Errorf("kv delete %s: %w", key, err)
	}
	return nil
}

// PutLoadBalancer stores a load balancer record.
func (s *Store) PutLoadBalancer(ctx context.Context, lb *LoadBalancerRecord) error {
	data, err := json.Marshal(lb)
	if err != nil {
		return fmt.Errorf("marshal load balancer: %w", err)
	}
	_, err = s.kv.Put(ctx, KeyPrefixLB+lb.LoadBalancerID, data)
	return err
}

// GetLoadBalancer retrieves a load balancer by its short ID.
func (s *Store) GetLoadBalancer(ctx context.Context, lbID string) (*LoadBalancerRecord, error) {
	entry, err := s.kv.Get(ctx, KeyPrefixLB+lbID)
	if err != nil {
		if errors.Is(err, jetstream.ErrKeyNotFound) {
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
func (s *Store) DeleteLoadBalancer(ctx context.Context, lbID string) error {
	err := s.kv.Delete(ctx, KeyPrefixLB+lbID)
	if err != nil && !errors.Is(err, jetstream.ErrKeyNotFound) {
		return err
	}
	return nil
}

// ListLoadBalancers returns all load balancer records.
func (s *Store) ListLoadBalancers(ctx context.Context) ([]*LoadBalancerRecord, error) {
	return listByPrefix[LoadBalancerRecord](ctx, s.kv, KeyPrefixLB)
}

// ListLoadBalancersStrict returns every load balancer record, failing if any
// record cannot be unmarshalled so the caller never treats a partial list as a
// complete inventory (the DNS reconcile prune authority requires the whole set).
func (s *Store) ListLoadBalancersStrict(ctx context.Context) ([]*LoadBalancerRecord, error) {
	return listByPrefixStrict[LoadBalancerRecord](ctx, s.kv, KeyPrefixLB)
}

// GetLoadBalancerByArn finds a load balancer by the short ID in the ARN's final
// path segment. Returns (nil, nil) if unparseable or not found. O(1) — Terraform
// hits Describe*Attributes on every plan/refresh.
func (s *Store) GetLoadBalancerByArn(ctx context.Context, arn string) (*LoadBalancerRecord, error) {
	// ELBv2 LB ARN: arn:aws:elasticloadbalancing:{region}:{account}:loadbalancer/{app,net}/{name}/{lbID}
	idx := strings.LastIndex(arn, "/")
	if idx < 0 || idx == len(arn)-1 {
		return nil, nil
	}
	lbID := arn[idx+1:]
	lb, err := s.GetLoadBalancer(ctx, lbID)
	if err != nil {
		return nil, err
	}
	if lb == nil {
		return nil, nil
	}
	// Defence-in-depth: ARN mismatch indicates KV corruption; never serve it silently.
	if lb.LoadBalancerArn != arn {
		slog.Error("load balancer KV record ARN mismatch",
			"requested_arn", arn, "stored_arn", lb.LoadBalancerArn, "lb_id", lbID)
		return nil, nil
	}
	return lb, nil
}

// GetLoadBalancerByName finds a load balancer by name within an account.
func (s *Store) GetLoadBalancerByName(ctx context.Context, name, accountID string) (*LoadBalancerRecord, error) {
	lbs, err := s.ListLoadBalancers(ctx)
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
func (s *Store) PutTargetGroup(ctx context.Context, tg *TargetGroupRecord) error {
	data, err := json.Marshal(tg)
	if err != nil {
		return fmt.Errorf("marshal target group: %w", err)
	}
	_, err = s.kv.Put(ctx, KeyPrefixTG+tg.TargetGroupID, data)
	return err
}

// GetTargetGroup retrieves a target group by its short ID.
func (s *Store) GetTargetGroup(ctx context.Context, tgID string) (*TargetGroupRecord, error) {
	entry, err := s.kv.Get(ctx, KeyPrefixTG+tgID)
	if err != nil {
		if errors.Is(err, jetstream.ErrKeyNotFound) {
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
func (s *Store) DeleteTargetGroup(ctx context.Context, tgID string) error {
	err := s.kv.Delete(ctx, KeyPrefixTG+tgID)
	if err != nil && !errors.Is(err, jetstream.ErrKeyNotFound) {
		return err
	}
	return nil
}

// ListTargetGroups returns all target group records.
func (s *Store) ListTargetGroups(ctx context.Context) ([]*TargetGroupRecord, error) {
	return listByPrefix[TargetGroupRecord](ctx, s.kv, KeyPrefixTG)
}

// GetTargetGroupByArn finds a target group by the short ID in the ARN's final
// path segment. O(1) — see GetLoadBalancerByArn for motivation.
func (s *Store) GetTargetGroupByArn(ctx context.Context, arn string) (*TargetGroupRecord, error) {
	// ELBv2 TG ARN: arn:aws:elasticloadbalancing:{region}:{account}:targetgroup/{name}/{tgID}
	idx := strings.LastIndex(arn, "/")
	if idx < 0 || idx == len(arn)-1 {
		return nil, nil
	}
	tgID := arn[idx+1:]
	tg, err := s.GetTargetGroup(ctx, tgID)
	if err != nil {
		return nil, err
	}
	if tg == nil {
		return nil, nil
	}
	if tg.TargetGroupArn != arn {
		slog.Error("target group KV record ARN mismatch",
			"requested_arn", arn, "stored_arn", tg.TargetGroupArn, "tg_id", tgID)
		return nil, nil
	}
	return tg, nil
}

// TargetGroupsForLB returns only the target groups attached to a load balancer
// via its listeners. It follows LB ID → LB ARN → listeners → TG ARNs → TGs.
func (s *Store) TargetGroupsForLB(ctx context.Context, lbID string) ([]*TargetGroupRecord, error) {
	lb, err := s.GetLoadBalancer(ctx, lbID)
	if err != nil {
		return nil, fmt.Errorf("get load balancer %s: %w", lbID, err)
	}
	if lb == nil {
		return nil, nil
	}

	listeners, err := s.ListListenersByLB(ctx, lb.LoadBalancerArn)
	if err != nil {
		return nil, fmt.Errorf("list listeners for %s: %w", lbID, err)
	}

	// Collect unique TG IDs from default actions and rule actions; rule TGs must
	// be included so the health checker can update state for all probed backends.
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
		rules, err := s.ListRulesByListener(ctx, l.ListenerArn)
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
		tg, err := s.GetTargetGroup(ctx, tgID)
		if err != nil {
			return nil, fmt.Errorf("get target group %s: %w", tgID, err)
		}
		if tg != nil {
			tgs = append(tgs, tg)
		}
	}
	return tgs, nil
}

// TargetGroupInUse reports whether any listener default action or rule action
// forwards to tgArn. A target group not referenced by a load balancer serves no
// traffic, so its targets are reported "unused" (AWS Target.NotInUse) instead of
// staying stuck in "initial" with no health checker to advance them.
func (s *Store) TargetGroupInUse(ctx context.Context, tgArn string) (bool, error) {
	if tgArn == "" {
		return false, nil
	}
	listeners, err := s.ListListeners(ctx)
	if err != nil {
		return false, err
	}
	for _, l := range listeners {
		for _, a := range l.DefaultActions {
			if a.TargetGroupArn == tgArn {
				return true, nil
			}
		}
	}
	rules, err := s.ListRules(ctx)
	if err != nil {
		return false, err
	}
	for _, r := range rules {
		for _, a := range r.Actions {
			if a.TargetGroupArn == tgArn {
				return true, nil
			}
		}
	}
	return false, nil
}

// GetTargetGroupByName finds a target group by name within a VPC.
func (s *Store) GetTargetGroupByName(ctx context.Context, name, vpcID string) (*TargetGroupRecord, error) {
	tgs, err := s.ListTargetGroups(ctx)
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
func (s *Store) PutListener(ctx context.Context, l *ListenerRecord) error {
	data, err := json.Marshal(l)
	if err != nil {
		return fmt.Errorf("marshal listener: %w", err)
	}
	_, err = s.kv.Put(ctx, KeyPrefixListener+l.ListenerID, data)
	return err
}

// GetListener retrieves a listener by its short ID.
func (s *Store) GetListener(ctx context.Context, listenerID string) (*ListenerRecord, error) {
	entry, err := s.kv.Get(ctx, KeyPrefixListener+listenerID)
	if err != nil {
		if errors.Is(err, jetstream.ErrKeyNotFound) {
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
func (s *Store) DeleteListener(ctx context.Context, listenerID string) error {
	err := s.kv.Delete(ctx, KeyPrefixListener+listenerID)
	if err != nil && !errors.Is(err, jetstream.ErrKeyNotFound) {
		return err
	}
	return nil
}

// ListListeners returns all listener records.
func (s *Store) ListListeners(ctx context.Context) ([]*ListenerRecord, error) {
	return listByPrefix[ListenerRecord](ctx, s.kv, KeyPrefixListener)
}

// ListListenersByLB returns all listeners for a specific load balancer ARN.
func (s *Store) ListListenersByLB(ctx context.Context, lbArn string) ([]*ListenerRecord, error) {
	all, err := s.ListListeners(ctx)
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

// GetListenerByArn finds a listener by the short ID in the ARN's final path segment.
// O(1) per ARN — Terraform calls DescribeTags for every listener on every plan.
func (s *Store) GetListenerByArn(ctx context.Context, arn string) (*ListenerRecord, error) {
	// ELBv2 listener ARN: arn:aws:elasticloadbalancing:{region}:{account}:listener/{app,net}/{name}/{lbID}/{listenerID}
	idx := strings.LastIndex(arn, "/")
	if idx < 0 || idx == len(arn)-1 {
		return nil, nil
	}
	listenerID := arn[idx+1:]
	l, err := s.GetListener(ctx, listenerID)
	if err != nil {
		return nil, err
	}
	if l == nil || l.ListenerArn != arn {
		return nil, nil
	}
	return l, nil
}

// --- Rule CRUD ---

// PutRule stores a rule record.
func (s *Store) PutRule(ctx context.Context, r *RuleRecord) error {
	data, err := json.Marshal(r)
	if err != nil {
		return fmt.Errorf("marshal rule: %w", err)
	}
	_, err = s.kv.Put(ctx, KeyPrefixRule+r.RuleID, data)
	return err
}

// GetRule retrieves a rule by its short ID.
func (s *Store) GetRule(ctx context.Context, ruleID string) (*RuleRecord, error) {
	entry, err := s.kv.Get(ctx, KeyPrefixRule+ruleID)
	if err != nil {
		if errors.Is(err, jetstream.ErrKeyNotFound) {
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
func (s *Store) DeleteRule(ctx context.Context, ruleID string) error {
	err := s.kv.Delete(ctx, KeyPrefixRule+ruleID)
	if err != nil && !errors.Is(err, jetstream.ErrKeyNotFound) {
		return err
	}
	return nil
}

// ListRules returns all rule records.
func (s *Store) ListRules(ctx context.Context) ([]*RuleRecord, error) {
	return listByPrefix[RuleRecord](ctx, s.kv, KeyPrefixRule)
}

// ListRulesByListener returns all rules attached to a listener ARN, sorted by
// ascending priority. Callers downstream of this method (HAProxy renderer,
// SetRulePriorities) rely on the sort.
func (s *Store) ListRulesByListener(ctx context.Context, listenerArn string) ([]*RuleRecord, error) {
	all, err := s.ListRules(ctx)
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

// GetRuleByArn finds a rule by its ARN via linear scan; the rule short ID is
// embedded after several listener-specific segments, making direct parsing brittle.
func (s *Store) GetRuleByArn(ctx context.Context, arn string) (*RuleRecord, error) {
	rules, err := s.ListRules(ctx)
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

// listByPrefix returns all records with keys matching the given prefix. A record
// that cannot be unmarshalled is logged and skipped so one corrupt entry does not
// fail read/describe paths.
func listByPrefix[T any](ctx context.Context, kv jetstream.KeyValue, prefix string) ([]*T, error) {
	return listByPrefixOpt[T](ctx, kv, prefix, false)
}

// listByPrefixStrict is listByPrefix but returns an error on the first record it
// cannot unmarshal, so a caller that treats the result as a complete inventory
// never mistakes a partial read for the whole set.
func listByPrefixStrict[T any](ctx context.Context, kv jetstream.KeyValue, prefix string) ([]*T, error) {
	return listByPrefixOpt[T](ctx, kv, prefix, true)
}

func listByPrefixOpt[T any](ctx context.Context, kv jetstream.KeyValue, prefix string, strict bool) ([]*T, error) {
	keys, err := kv.Keys(ctx)
	if err != nil {
		if errors.Is(err, jetstream.ErrNoKeysFound) {
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

		entry, err := kv.Get(ctx, key)
		if err != nil {
			if errors.Is(err, jetstream.ErrKeyNotFound) {
				continue
			}
			return nil, err
		}

		var record T
		if err := json.Unmarshal(entry.Value(), &record); err != nil {
			if strict {
				return nil, fmt.Errorf("unmarshal record %q: %w", key, err)
			}
			slog.Error("Failed to unmarshal ELBv2 record", "key", key, "err", err)
			continue
		}

		result = append(result, &record)
	}

	return result, nil
}
