package gateway_ec2_instance

import (
	"context"
	"log/slog"
	"sort"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	handlers_ec2_instance "github.com/mulgadc/spinifex/spinifex/handlers/ec2/instance"
	"github.com/mulgadc/spinifex/spinifex/instancecache"
	"github.com/mulgadc/spinifex/spinifex/otelsetup"
	"github.com/mulgadc/spinifex/spinifex/vm"
)

// DivergenceClass is why one shadow comparison disagreed with the fan-out.
// Assigned at the point a divergence is logged, never afterward: "zero
// divergences" is not the gate this mode is judged on, so every entry must
// already carry the reason it either blocks a flip to cache-served answers
// or does not.
type DivergenceClass string

const (
	// DivergenceExpectedFix is the cache correctly holding an instance whose
	// owning node is not currently answering the fan-out — the reason this
	// cache path exists.
	DivergenceExpectedFix DivergenceClass = "expected_fix"
	// DivergenceExpectedPropagation is a divergence on an instance recent
	// enough that the cache has not caught up yet. "Fine" only while it
	// keeps converging; see DivergencePersistent.
	DivergenceExpectedPropagation DivergenceClass = "expected_propagation"
	// DivergenceExpectedDrainWindow is a drain-stopped instance the fan-out
	// still reports and the cache path deliberately excludes, on a node
	// mid-drain.
	DivergenceExpectedDrainWindow DivergenceClass = "expected_drain_window"
	// DivergenceExpectedVisibilityTightening is a ManagedBy instance the
	// fan-out's stopped path still shows a non-Global caller and the cache
	// path correctly hides — the deliberate visibility fix, working.
	DivergenceExpectedVisibilityTightening DivergenceClass = "expected_visibility_tightening"
	// DivergenceUnexplained has no structural account for it: the two
	// disagree with no node event, or a live-node field mismatch is still
	// present after its (short) propagation grace. Blocks the flip.
	DivergenceUnexplained DivergenceClass = "unexplained"
	// DivergencePersistent is a divergence whose "fine" verdict depended on
	// convergence (DivergenceExpectedPropagation) that is still present
	// after one resync interval. Blocks the flip.
	DivergencePersistent DivergenceClass = "persistent"
)

// Snapshot is a plain instance-ID-to-state-name extraction of a served
// DescribeInstances answer. It exists so the shadow path never holds a
// reference into the *ec2.DescribeInstancesOutput actually served: that
// output keeps getting mutated in place after the handler returns
// (marshalEC2Response's NormalizeXMLOutput rewrites nil slices on it), so
// anything crossing into the detached shadow goroutine must be a copy taken
// before the handler returns.
type Snapshot map[string]string

// SnapshotDescribeOutput extracts a Snapshot from a served answer. Call this
// synchronously in the request path, before handing anything to
// ShadowComparator.Run.
func SnapshotDescribeOutput(out *ec2.DescribeInstancesOutput) Snapshot {
	snap := Snapshot{}
	if out == nil {
		return snap
	}
	for _, res := range out.Reservations {
		if res == nil {
			continue
		}
		for _, inst := range res.Instances {
			if inst == nil || inst.InstanceId == nil {
				continue
			}
			snap[aws.StringValue(inst.InstanceId)] = instanceStateName(inst)
		}
	}
	return snap
}

func instanceStateName(inst *ec2.Instance) string {
	if inst.State == nil {
		return ""
	}
	return aws.StringValue(inst.State.Name)
}

// divergence is one instance- or field-level disagreement found during a
// shadow comparison, already classified by the time it is built.
type divergence struct {
	AccountID   string
	InstanceID  string
	Field       string // "presence" or a projected field name, e.g. "state"
	FanoutValue string
	CacheValue  string
	Class       DivergenceClass
	NodeState   instancecache.NodeState
	Age         time.Duration
}

// rawDiff is the unclassified output of comparing two snapshots.
type rawDiff struct {
	fanoutOnly []string
	cacheOnly  []string
	mismatched []string
}

// diffSnapshots buckets every instance ID into exactly one of: present only
// in the fan-out's answer, present only in the cache's, or present in both
// with a differing state. Sorted for deterministic logging order.
func diffSnapshots(fanoutSnap, cacheSnap Snapshot) rawDiff {
	var d rawDiff
	for id, fs := range fanoutSnap {
		cs, ok := cacheSnap[id]
		if !ok {
			d.fanoutOnly = append(d.fanoutOnly, id)
			continue
		}
		if fs != cs {
			d.mismatched = append(d.mismatched, id)
		}
	}
	for id := range cacheSnap {
		if _, ok := fanoutSnap[id]; !ok {
			d.cacheOnly = append(d.cacheOnly, id)
		}
	}
	sort.Strings(d.fanoutOnly)
	sort.Strings(d.cacheOnly)
	sort.Strings(d.mismatched)
	return d
}

// maxTrackedDivergences bounds divergenceTracker's memory when an account
// never issues the whole-account listing that lets a resolved key be pruned
// by scope. It is a backstop, not the normal path: the normal path prunes a
// key the moment a shadow run rechecks its scope and finds it settled.
// Deliberately untested: exercising eviction means driving 8192+ entries, a
// memory backstop rather than a correctness path this mode is judged on.
const maxTrackedDivergences = 8192

// divergenceKey identifies one tracked divergence: an instance, and which
// aspect of it diverged. Two fields on the same instance are independent
// keys, so one settling does not erase the other's history.
type divergenceKey struct {
	accountID  string
	instanceID string
	field      string
}

// divergenceTracker upgrades a convergence-dependent divergence
// (DivergenceExpectedPropagation) to DivergencePersistent once it has been
// observed continuously for at least one resync interval, and forgets a key
// the instant a shadow run that covers it finds it settled. Bounded by that
// prune-on-recheck behaviour in the common case; the entry cap is a backstop
// for an account that only ever issues explicit-ID requests naming
// instances it never rechecks.
type divergenceTracker struct {
	mu        sync.Mutex
	firstSeen map[divergenceKey]time.Time
	order     []divergenceKey
}

func newDivergenceTracker() *divergenceTracker {
	return &divergenceTracker{firstSeen: map[divergenceKey]time.Time{}}
}

// observe upserts key and reports how long it has been continuously
// diverging (zero on first observation). Only called for a divergence whose
// "fine" verdict depends on convergence; a structurally-explained divergence
// never enters the tracker.
func (t *divergenceTracker) observe(key divergenceKey, now time.Time) time.Duration {
	t.mu.Lock()
	defer t.mu.Unlock()
	first, ok := t.firstSeen[key]
	if !ok {
		if len(t.order) >= maxTrackedDivergences {
			oldest := t.order[0]
			t.order = t.order[1:]
			delete(t.firstSeen, oldest)
		}
		t.firstSeen[key] = now
		t.order = append(t.order, key)
		return 0
	}
	return now.Sub(first)
}

// resolve drops key: a shadow run whose scope covered it just found it no
// longer diverges. A no-op if key was never tracked (a structural
// divergence, or one already resolved).
func (t *divergenceTracker) resolve(key divergenceKey) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if _, ok := t.firstSeen[key]; !ok {
		return
	}
	delete(t.firstSeen, key)
	for i, k := range t.order {
		if k == key {
			t.order = append(t.order[:i], t.order[i+1:]...)
			break
		}
	}
}

// keysForAccount lists every key currently tracked for accountID, so a
// whole-account shadow run can prune anything it did not rediscover.
func (t *divergenceTracker) keysForAccount(accountID string) []divergenceKey {
	t.mu.Lock()
	defer t.mu.Unlock()
	var out []divergenceKey
	for k := range t.firstSeen {
		if k.accountID == accountID {
			out = append(out, k)
		}
	}
	return out
}

// maxConcurrentShadowRuns bounds how many shadow comparisons may be in
// flight at once — the same defence DescribeInstances' own absence-proof
// path uses (see maxConcurrentAbsenceProofs): a slow cache degrades to
// "skipped" under load, never to a growing goroutine backlog.
const maxConcurrentShadowRuns = 32

// describeShadowResyncInterval is the default convergence window a
// DivergenceExpectedPropagation entry gets before ShadowComparator
// reclassifies it as DivergencePersistent. Callers wiring a real cache
// should pass its actual resync interval instead; see NewShadowComparator.
const describeShadowResyncInterval = instancecache.DefaultResyncInterval

// describeShadowLiveMismatchGraceDivisor shrinks the resync interval into
// the grace a field mismatch gets on a live, answering node: the KV write
// behind a cache update follows the node's own change over a watch that
// delivers in milliseconds, so a live node should converge in a small
// fraction of one resync cycle. A full interval would let an ordinary
// write race sit unflagged for far longer than it ever needs to.
const describeShadowLiveMismatchGraceDivisor = 6

// ShadowComparator runs the cache-served describe path alongside an
// already-served fan-out answer, purely to compare and log; it never
// returns anything a caller could serve. One instance is shared for the
// gateway's lifetime so its divergence tracker persists across requests,
// which persistence classification depends on.
type ShadowComparator struct {
	tracker        *divergenceTracker
	resyncInterval time.Duration
	slots          chan struct{}
}

// NewShadowComparator builds a comparator that treats a divergence as
// persistent once it has been continuously observed for resyncInterval — the
// cache's own resync cadence, so "unresolved for one resync" matches what
// the cache can actually be expected to have caught up on. A non-positive
// value falls back to instancecache.DefaultResyncInterval.
func NewShadowComparator(resyncInterval time.Duration) *ShadowComparator {
	if resyncInterval <= 0 {
		resyncInterval = describeShadowResyncInterval
	}
	return &ShadowComparator{
		tracker:        newDivergenceTracker(),
		resyncInterval: resyncInterval,
		slots:          make(chan struct{}, maxConcurrentShadowRuns),
	}
}

// Run compares the cache path's answer for input against fanoutSnap — a
// snapshot of the answer already served to the customer — and logs each
// divergence with its classification. It never returns anything and never
// touches what was served; a panic anywhere in the comparison (including in
// cache) is recovered so it can never surface to the caller, which by the
// time this runs already has its answer. ctx must be detached from the
// request (context.WithoutCancel) and carry its own deadline: the request
// this shadows may finish, and its context be cancelled, before Run does.
func (s *ShadowComparator) Run(ctx context.Context, cache CacheReader, liveness nodeLiveness, input *ec2.DescribeInstancesInput, accountID, az string, fanoutSnap Snapshot) {
	// A nil receiver would panic on the field read below, before the
	// recover deferred further down ever got a chance to register. Guard it
	// here rather than rely on every caller's own nil check: this runs
	// detached in its own goroutine, so an unrecovered panic here would
	// crash the process, not just this comparison.
	if s == nil {
		return
	}

	select {
	case s.slots <- struct{}{}:
		defer func() { <-s.slots }()
	default:
		slog.InfoContext(ctx, "DescribeInstances shadow: skipped, concurrency budget exhausted",
			"limit", maxConcurrentShadowRuns)
		return
	}

	defer func() {
		if r := recover(); r != nil {
			slog.ErrorContext(ctx, "DescribeInstances shadow: recovered from panic",
				"account", accountID, "panic", r)
		}
	}()

	cacheOut, _, err := DescribeFromCache(ctx, cache, input, accountID, az)
	if err != nil {
		slog.WarnContext(ctx, "DescribeInstances shadow: cache path errored", "account", accountID, "err", err)
		return
	}
	if cacheOut == nil {
		// No answer: a cold cache or a not-yet-resolvable request. Nothing to
		// compare, and this is not itself a divergence.
		return
	}

	cacheSnap := SnapshotDescribeOutput(cacheOut)
	diff := diffSnapshots(fanoutSnap, cacheSnap)

	now := time.Now()
	found := map[divergenceKey]bool{}

	for _, id := range diff.fanoutOnly {
		d, key := s.evalFanoutOnly(ctx, cache, liveness, accountID, id, now)
		s.log(ctx, d)
		if key != nil {
			found[*key] = true
		}
	}
	for _, id := range diff.cacheOnly {
		d, key := s.evalCacheOnly(ctx, cache, liveness, accountID, id, now)
		s.log(ctx, d)
		if key != nil {
			found[*key] = true
		}
	}
	for _, id := range diff.mismatched {
		d, key := s.evalMismatch(ctx, cache, liveness, accountID, id, fanoutSnap[id], cacheSnap[id], now)
		s.log(ctx, d)
		if key != nil {
			found[*key] = true
		}
	}

	s.pruneTracked(accountID, input, found)
}

// pruneTracked resolves every tracked key this run's scope covered but did
// not rediscover as diverging. Scope is the whole account for a filters-only
// request (List covers it completely) and exactly the requested IDs for an
// explicit-ID request, matching what DescribeFromCache itself can settle in
// each case.
func (s *ShadowComparator) pruneTracked(accountID string, input *ec2.DescribeInstancesInput, found map[divergenceKey]bool) {
	var keys []divergenceKey
	if len(input.InstanceIds) == 0 {
		keys = s.tracker.keysForAccount(accountID)
	} else {
		for _, idPtr := range input.InstanceIds {
			if idPtr == nil || *idPtr == "" {
				continue
			}
			keys = append(keys,
				divergenceKey{accountID, *idPtr, "presence"},
				divergenceKey{accountID, *idPtr, "state"})
		}
	}
	for _, k := range keys {
		if !found[k] {
			s.tracker.resolve(k)
		}
	}
}

// evalFanoutOnly classifies an instance the fan-out served that the cache
// path did not. A structural, permanent explanation (drain window,
// visibility tightening) wins outright; otherwise it is a propagation
// candidate tracked by key so a later run can tell whether it converged.
func (s *ShadowComparator) evalFanoutOnly(ctx context.Context, cache CacheReader, liveness nodeLiveness, accountID, id string, now time.Time) (divergence, *divergenceKey) {
	v, _ := cache.Get(ctx, id)

	if v != nil && v.Status == vm.StateStopped && !v.OperatorStopped() {
		return divergence{AccountID: accountID, InstanceID: id, Field: "presence",
			FanoutValue: "present", CacheValue: "absent",
			Class: DivergenceExpectedDrainWindow, NodeState: liveState(ctx, liveness, v.LastNode)}, nil
	}
	if v != nil && !handlers_ec2_instance.IsInstanceVisibleToCaller(accountID, v) {
		return divergence{AccountID: accountID, InstanceID: id, Field: "presence",
			FanoutValue: "present", CacheValue: "absent",
			Class: DivergenceExpectedVisibilityTightening, NodeState: liveState(ctx, liveness, v.LastNode)}, nil
	}

	key := divergenceKey{accountID, id, "presence"}
	age := s.tracker.observe(key, now)
	state := instancecache.NodeUnknown
	if v != nil {
		state = liveState(ctx, liveness, v.LastNode)
	}
	class := DivergenceExpectedPropagation
	if age >= s.resyncInterval {
		class = DivergencePersistent
	}
	return divergence{AccountID: accountID, InstanceID: id, Field: "presence",
		FanoutValue: "present", CacheValue: "absent", Class: class, NodeState: state, Age: age}, &key
}

// evalCacheOnly classifies an instance the cache path served that the
// fan-out did not. The owning node not answering the fan-out is exactly what
// this cache path exists to paper over; anything else is a propagation
// candidate.
func (s *ShadowComparator) evalCacheOnly(ctx context.Context, cache CacheReader, liveness nodeLiveness, accountID, id string, now time.Time) (divergence, *divergenceKey) {
	v, err := cache.Get(ctx, id)
	if err == nil && v != nil {
		state := liveState(ctx, liveness, v.LastNode)
		if state != instancecache.NodeLive {
			return divergence{AccountID: accountID, InstanceID: id, Field: "presence",
				FanoutValue: "absent", CacheValue: "present",
				Class: DivergenceExpectedFix, NodeState: state}, nil
		}
	}

	key := divergenceKey{accountID, id, "presence"}
	age := s.tracker.observe(key, now)
	state := instancecache.NodeUnknown
	if v != nil {
		state = liveState(ctx, liveness, v.LastNode)
	}
	class := DivergenceExpectedPropagation
	if age >= s.resyncInterval {
		class = DivergencePersistent
	}
	return divergence{AccountID: accountID, InstanceID: id, Field: "presence",
		FanoutValue: "absent", CacheValue: "present", Class: class, NodeState: state, Age: age}, &key
}

// evalMismatch classifies an instance both sides served with a differing
// state. The KV write behind a cache update follows the node's own state
// change, so even a live, answering node has an ordinary propagation
// window — it just gets a shorter grace than a not-live one, since the
// watch behind it delivers in milliseconds. A live-node mismatch that
// outlives its grace is unexplained rather than persistent: it should have
// converged well inside one resync, so surviving says something is wrong,
// not just slow.
func (s *ShadowComparator) evalMismatch(ctx context.Context, cache CacheReader, liveness nodeLiveness, accountID, id, fanoutValue, cacheValue string, now time.Time) (divergence, *divergenceKey) {
	v, err := cache.Get(ctx, id)
	state := instancecache.NodeUnknown
	if err == nil && v != nil {
		state = liveState(ctx, liveness, v.LastNode)
	}

	key := divergenceKey{accountID, id, "state"}
	age := s.tracker.observe(key, now)

	deadline := s.resyncInterval
	survivorClass := DivergencePersistent
	if state == instancecache.NodeLive {
		deadline = s.resyncInterval / describeShadowLiveMismatchGraceDivisor
		survivorClass = DivergenceUnexplained
	}

	class := DivergenceExpectedPropagation
	if age >= deadline {
		class = survivorClass
	}
	return divergence{AccountID: accountID, InstanceID: id, Field: "state",
		FanoutValue: fanoutValue, CacheValue: cacheValue, Class: class, NodeState: state, Age: age}, &key
}

// liveState answers Unknown for a node with no name rather than asking
// liveness at all, matching Liveness.State's own zero-node handling.
func liveState(ctx context.Context, liveness nodeLiveness, node string) instancecache.NodeState {
	if liveness == nil || node == "" {
		return instancecache.NodeUnknown
	}
	return liveness.State(ctx, node)
}

func nodeStateString(s instancecache.NodeState) string {
	switch s {
	case instancecache.NodeLive:
		return "live"
	case instancecache.NodeStale:
		return "stale"
	default:
		return "unknown"
	}
}

// log records one divergence at Info for a structurally-explained class and
// Warn for the two that block a flip to cache-served answers, so the ones
// that matter are triageable by severity alone.
func (s *ShadowComparator) log(ctx context.Context, d divergence) {
	attrs := []any{
		"account", d.AccountID,
		"instance_id", d.InstanceID,
		"field", d.Field,
		"fanout_value", d.FanoutValue,
		"cache_value", d.CacheValue,
		"class", string(d.Class),
		"node_state", nodeStateString(d.NodeState),
		"divergence_age_ms", otelsetup.Millis(d.Age),
	}
	if d.Class == DivergenceUnexplained || d.Class == DivergencePersistent {
		slog.WarnContext(ctx, "DescribeInstances shadow: divergence", attrs...)
		return
	}
	slog.InfoContext(ctx, "DescribeInstances shadow: divergence", attrs...)
}
