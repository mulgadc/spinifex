package handlers_bedrock

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/mulgadc/spinifex/spinifex/awserrors"
	"github.com/mulgadc/spinifex/spinifex/config"
	gateway_bedrock "github.com/mulgadc/spinifex/spinifex/gateway/bedrock"
	"github.com/mulgadc/spinifex/spinifex/utils"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
)

// EndpointService is the serving-endpoint lifecycle surface, implemented both
// by the daemon-side Service (the sole KV writer, D2) and by
// NATSEndpointService (the NATS-forwarding client gateway-side code and tests
// use instead of importing this package's JetStream/launch dependencies).
type EndpointService interface {
	Ensure(ctx context.Context, in *EnsureEndpointInput, accountID string) (*EnsureEndpointOutput, error)
	Describe(ctx context.Context, in *DescribeEndpointInput, accountID string) (*DescribeEndpointOutput, error)
	List(ctx context.Context, in *ListEndpointsInput, accountID string) (*ListEndpointsOutput, error)
	Delete(ctx context.Context, in *DeleteEndpointInput, accountID string) (*DeleteEndpointOutput, error)
}

// ServiceDeps are the daemon-supplied collaborators the Service needs beyond
// NATS: the launch/VPC/volume plumbing and the GPU capacity view.
type ServiceDeps struct {
	Config *config.Config
	Launch LaunchDeps
	GPU    gpuSnapshotter
	// NodeID identifies which daemon replica's node a launched VM lands on
	// (stamped into EndpointRecord.NodeID).
	NodeID string
	// StartupTimeout bounds the readiness probe; zero takes defaultStartupTimeout.
	StartupTimeout time.Duration
	// HTTPClient is the readiness-probe client; nil takes a zero-value
	// *http.Client (context-deadline-bound, no extra per-request timeout needed).
	HTTPClient *http.Client
	// PollInterval overrides readinessPollInterval; zero takes the default.
	// Test-only knob so a readiness test does not wait out the production cadence.
	PollInterval time.Duration
	// Replicas sizes the bedrock-endpoints KV bucket on first create.
	Replicas int
	// MinResidency protects a freshly READY endpoint from eviction; zero takes
	// defaultMinResidency.
	MinResidency time.Duration
}

// Service is the daemon-side, KV-backed handler set for the serving-endpoint
// lifecycle. One per daemon; every replica subscribes the same NATS subjects
// on a shared queue group, so exactly one replica handles a given request, but
// any replica may read/write any key — hence the KV CAS layer beneath the
// local per-key mutex (see ensureMu).
type Service struct {
	nc   *nats.Conn
	deps ServiceDeps

	// ensureMu collapses concurrent Ensure calls FOR THE SAME KEY on THIS
	// replica into one KV round trip: without it, N concurrent local callers
	// all hit kv.Create and only the loser count changes, wasting N-1 network
	// round trips on every burst. It does nothing for concurrent Ensures
	// landing on different replicas — that race is only ever resolved by the
	// KV Create/CAS below, which is why both layers are required.
	ensureMu keyMutex

	// launchWG tracks in-flight async launches for test determinism (WaitLaunches).
	launchWG sync.WaitGroup
}

var _ EndpointService = (*Service)(nil)

// NewService constructs a Service bound to nc, using deps for launch/capacity.
func NewService(nc *nats.Conn, deps ServiceDeps) *Service {
	return &Service{nc: nc, deps: deps}
}

// WaitLaunches blocks until every async launch this Service has started so
// far has finished. Test-only.
func (s *Service) WaitLaunches() { s.launchWG.Wait() }

func (s *Service) js() (jetstream.JetStream, error) {
	if s.nc == nil {
		return nil, errors.New("bedrock service: nil nats connection")
	}
	return jetstream.New(s.nc)
}

func (s *Service) bucket(ctx context.Context) (jetstream.KeyValue, error) {
	js, err := s.js()
	if err != nil {
		return nil, err
	}
	return GetOrCreateEndpointsBucket(ctx, js, s.deps.Replicas)
}

func (s *Service) startupTimeout() time.Duration {
	if s.deps.StartupTimeout > 0 {
		return s.deps.StartupTimeout
	}
	return defaultStartupTimeout
}

func (s *Service) pollInterval() time.Duration {
	if s.deps.PollInterval > 0 {
		return s.deps.PollInterval
	}
	return readinessPollInterval
}

func (s *Service) minResidency() time.Duration {
	if s.deps.MinResidency > 0 {
		return s.deps.MinResidency
	}
	return defaultMinResidency
}

func (s *Service) httpClient() *http.Client {
	if s.deps.HTTPClient != nil {
		return s.deps.HTTPClient
	}
	return &http.Client{}
}

// resolveAccountID returns accountID, or utils.GlobalAccountID for the zero
// value, so every existing Ensure/Describe/Delete caller that leaves the
// input's AccountID unset keeps resolving to the shared platform endpoint.
func resolveAccountID(accountID string) string {
	if accountID == "" {
		return utils.GlobalAccountID
	}
	return accountID
}

// Ensure idempotently guarantees modelID has a running or starting endpoint.
// A model with no serving spec (unknown, or a provider entry) is rejected
// before any KV touch. Capacity is admission-checked before the claim so a
// refusal never leaves a STARTING record for a concurrent caller to observe.
func (s *Service) Ensure(ctx context.Context, in *EnsureEndpointInput, _ string) (*EnsureEndpointOutput, error) {
	if in == nil || in.ModelID == "" {
		return nil, errors.New(awserrors.ErrorInvalidParameterValue)
	}
	spec, specFound, selfHost := gateway_bedrock.LookupServingSpec(in.ModelID)
	if !specFound || !selfHost {
		return nil, errors.New(awserrors.ErrorResourceNotFoundException)
	}

	// Serving endpoints are shared platform infra by default: an Ensure with
	// no AccountID converges on one shared VM stored under the system
	// account. A caller that supplies its own AccountID (a pinned,
	// account-scoped endpoint) gets a distinct key instead.
	storeAccountID := resolveAccountID(in.AccountID)
	key := EndpointKey(storeAccountID, in.ModelID)

	unlock := s.ensureMu.lock(key)
	defer unlock()

	kv, err := s.bucket(ctx)
	if err != nil {
		return nil, err
	}

	var existing EndpointRecord
	found, err := getJSON(ctx, kv, key, &existing)
	if err != nil {
		return nil, fmt.Errorf("bedrock: read endpoint %s: %w", in.ModelID, err)
	}
	if found {
		switch existing.State {
		case StateStarting, StateReady:
			return &EnsureEndpointOutput{Endpoint: existing}, nil
		case StateDraining:
			return nil, awserrors.Errorf(awserrors.ErrorModelNotReadyException,
				"bedrock: endpoint for %s is draining", in.ModelID)
		}
	}

	if err := admitCapacity(s.deps.GPU, spec.MinVRAMMiB); err != nil {
		// No free device. A GPU held by a model nobody is calling is not a
		// reason to refuse this one, so make room and re-check — but return the
		// original refusal, unchanged, when nothing can be given up.
		if !s.evictForCapacity(ctx, kv, in.ModelID, spec.MinVRAMMiB) {
			return nil, err
		}
		if retryErr := admitCapacity(s.deps.GPU, spec.MinVRAMMiB); retryErr != nil {
			return nil, retryErr
		}
	}
	if err := validateTransition(StateAbsent, StateStarting); err != nil {
		return nil, err
	}

	rec := EndpointRecord{
		AccountID:  storeAccountID,
		ModelID:    in.ModelID,
		State:      StateStarting,
		NodeID:     s.deps.NodeID,
		CreatedAt:  time.Now().UTC(),
		Generation: 1,
		Pinned:     in.Pinned,
	}
	if _, err := createJSONRevision(ctx, kv, key, rec); err != nil {
		if errors.Is(err, jetstream.ErrKeyExists) {
			// Lost the cross-replica race after the read above: some other
			// replica's Ensure claimed the key in between. Its record is the
			// answer, not an error.
			var winner EndpointRecord
			if ok, gerr := getJSON(ctx, kv, key, &winner); gerr == nil && ok {
				return &EnsureEndpointOutput{Endpoint: winner}, nil
			}
		}
		return nil, fmt.Errorf("bedrock: claim endpoint %s: %w", in.ModelID, err)
	}

	// The launch outlives the request, so it runs on its own background
	// context rather than the caller's, which is cancelled once this reply is
	// written.
	launchCtx := context.Background()
	s.launchWG.Go(func() {
		defer func() {
			if r := recover(); r != nil {
				slog.ErrorContext(launchCtx, "bedrock: launch panic", "model", in.ModelID, "panic", r)
				s.abortLaunch(launchCtx, key, in.ModelID)
			}
		}()
		s.runLaunch(launchCtx, key, rec, spec)
	})

	return &EnsureEndpointOutput{Endpoint: rec}, nil
}

// evictForCapacity stops the least recently used evictable endpoint so a
// pending launch can have its device, reporting whether one was stopped. The
// eviction goes through Delete, so the device is released by the same
// DRAINING -> terminate -> release path an operator delete takes rather than
// being taken from underneath a running process.
//
// wantModelID is excluded defensively: a caller that reached the capacity
// check has no endpoint of its own to evict, and stopping one to launch it
// again would be a no-op at best.
//
// Delete takes the per-key mutex for the victim while Ensure holds it for
// wantModelID. The two can never be the same key, so the locks are disjoint.
func (s *Service) evictForCapacity(ctx context.Context, kv jetstream.KeyValue, wantModelID string, minVRAMMiB int) bool {
	recs, err := ListEndpoints(ctx, kv, utils.GlobalAccountID)
	if err != nil {
		slog.ErrorContext(ctx, "bedrock: list endpoints for eviction failed", "model", wantModelID, "err", err)
		return false
	}
	candidates := make([]EndpointRecord, 0, len(recs))
	for _, rec := range recs {
		if rec.ModelID != wantModelID {
			candidates = append(candidates, rec)
		}
	}

	victim, found := selectEvictable(candidates, s.deps.NodeID, s.minResidency(), time.Now().UTC())
	if !found {
		slog.InfoContext(ctx, "bedrock: no free GPU and nothing evictable",
			"model", wantModelID, "minVRAMMiB", minVRAMMiB, "endpoints", len(recs))
		return false
	}

	slog.InfoContext(ctx, "bedrock: evicting an idle endpoint to make room",
		"evicting", victim.ModelID, "instanceId", victim.InstanceID, "for", wantModelID,
		"idleSince", victim.LastActive())
	if _, err := s.Delete(ctx, &DeleteEndpointInput{ModelID: victim.ModelID}, utils.GlobalAccountID); err != nil {
		slog.ErrorContext(ctx, "bedrock: eviction failed; the pending launch keeps its capacity refusal",
			"evicting", victim.ModelID, "for", wantModelID, "err", err)
		return false
	}
	return true
}

// runLaunch performs the slow launch + readiness probe and writes the
// terminal state. Only this goroutine writes key while the record is
// STARTING (guaranteed by Ensure returning early for any concurrent caller
// that observes STARTING), so no CAS-conflict retry loop is needed here.
func (s *Service) runLaunch(ctx context.Context, key string, rec EndpointRecord, spec gateway_bedrock.ServingSpec) {
	out, err := LaunchServingVM(ctx, s.deps.Launch, LaunchInput{
		ModelID:      rec.ModelID,
		InstanceType: spec.InstanceType,
		VLLMArgs:     spec.VLLMArgs,
	})
	if err != nil {
		slog.ErrorContext(ctx, "bedrock: launch serving VM failed", "model", rec.ModelID, "err", err)
		s.abortLaunch(ctx, key, rec.ModelID)
		return
	}

	rec.InstanceID = out.InstanceID
	rec.ENIID = out.ENIID
	rec.WeightsVolumeID = out.WeightsVolumeID
	rec.BaseURL = out.BaseURL

	timeoutCtx, cancel := context.WithTimeout(ctx, s.startupTimeout())
	defer cancel()
	if err := waitReady(timeoutCtx, s.httpClient(), rec.BaseURL, s.pollInterval()); err != nil {
		slog.ErrorContext(ctx, "bedrock: readiness probe timed out; unwinding launch",
			"model", rec.ModelID, "instanceId", rec.InstanceID, "err", err)
		out.Unwind(ctx)
		s.abortLaunch(ctx, key, rec.ModelID)
		return
	}

	if err := validateTransition(StateStarting, StateReady); err != nil {
		slog.ErrorContext(ctx, "bedrock: illegal transition to READY", "model", rec.ModelID, "err", err)
		return
	}
	rec.State = StateReady
	rec.ReadyAt = time.Now().UTC()
	rec.Generation++

	kv, err := s.bucket(ctx)
	if err != nil {
		slog.ErrorContext(ctx, "bedrock: bucket unavailable to record READY", "model", rec.ModelID, "err", err)
		return
	}
	_, rev, gerr := readCurrent(ctx, kv, key)
	if gerr != nil {
		slog.ErrorContext(ctx, "bedrock: re-read endpoint before READY write failed", "model", rec.ModelID, "err", gerr)
		return
	}
	if err := updateJSON(ctx, kv, key, rev, rec); err != nil {
		slog.ErrorContext(ctx, "bedrock: CAS write of READY state failed", "model", rec.ModelID, "err", err)
	}
}

// abortLaunch reverts a STARTING record to ABSENT (deletes the key) so a
// retried Ensure gets a clean claim instead of a dead record. Runs on the
// launch's own background context: the reader that triggered this failure
// may already be gone, but the revert must still happen.
func (s *Service) abortLaunch(ctx context.Context, key, modelID string) {
	if err := validateTransition(StateStarting, StateAbsent); err != nil {
		slog.ErrorContext(ctx, "bedrock: illegal abort transition", "model", modelID, "err", err)
		return
	}
	kv, err := s.bucket(ctx)
	if err != nil {
		slog.ErrorContext(ctx, "bedrock: bucket unavailable to abort launch", "model", modelID, "err", err)
		return
	}
	if err := deleteJSON(ctx, kv, key); err != nil {
		slog.ErrorContext(ctx, "bedrock: revert of failed launch failed; record may be stuck STARTING",
			"model", modelID, "err", err)
	}
}

// readCurrent re-reads key's current record and revision.
func readCurrent(ctx context.Context, kv jetstream.KeyValue, key string) (EndpointRecord, uint64, error) {
	var rec EndpointRecord
	rev, found, err := getJSONRevision(ctx, kv, key, &rec)
	if err != nil {
		return EndpointRecord{}, 0, err
	}
	if !found {
		return EndpointRecord{}, 0, fmt.Errorf("bedrock: endpoint key %s vanished mid-launch", key)
	}
	return rec, rev, nil
}

// Describe returns modelID's current endpoint record, or a synthetic ABSENT
// record when none exists — never an error, since "not provisioned" is a
// normal answer, not a fault.
func (s *Service) Describe(ctx context.Context, in *DescribeEndpointInput, _ string) (*DescribeEndpointOutput, error) {
	if in == nil || in.ModelID == "" {
		return nil, errors.New(awserrors.ErrorInvalidParameterValue)
	}
	storeAccountID := resolveAccountID(in.AccountID)
	kv, err := s.bucket(ctx)
	if err != nil {
		return nil, err
	}
	var rec EndpointRecord
	found, err := getJSON(ctx, kv, EndpointKey(storeAccountID, in.ModelID), &rec)
	if err != nil {
		return nil, fmt.Errorf("bedrock: describe endpoint %s: %w", in.ModelID, err)
	}
	if !found {
		rec = EndpointRecord{AccountID: storeAccountID, ModelID: in.ModelID, State: StateAbsent}
	}
	return &DescribeEndpointOutput{Endpoint: rec}, nil
}

// List returns every endpoint record in the shared endpoints bucket, across
// every account: an operator listing must see a pinned, account-scoped
// endpoint alongside the shared platform ones, not just the latter.
func (s *Service) List(ctx context.Context, _ *ListEndpointsInput, _ string) (*ListEndpointsOutput, error) {
	kv, err := s.bucket(ctx)
	if err != nil {
		return nil, err
	}
	recs, err := ListAllEndpoints(ctx, kv)
	if err != nil {
		return nil, err
	}
	return &ListEndpointsOutput{Endpoints: recs}, nil
}

// Delete moves a READY endpoint to DRAINING, tears down its VM/ENI/weights
// volume, then deletes the record. Runs synchronously: unlike Ensure this is
// an explicit, infrequent operator action, so there is no client-latency
// reason to background it. An already-ABSENT endpoint is a no-op success, and
// a DRAINING one resumes: a teardown that failed partway must be retryable,
// or the record is stranded in a state nothing else clears.
func (s *Service) Delete(ctx context.Context, in *DeleteEndpointInput, _ string) (*DeleteEndpointOutput, error) {
	if in == nil || in.ModelID == "" {
		return nil, errors.New(awserrors.ErrorInvalidParameterValue)
	}
	storeAccountID := resolveAccountID(in.AccountID)
	key := EndpointKey(storeAccountID, in.ModelID)

	unlock := s.ensureMu.lock(key)
	defer unlock()

	kv, err := s.bucket(ctx)
	if err != nil {
		return nil, err
	}
	rec, rev, found, err := getFullJSON(ctx, kv, key)
	if err != nil {
		return nil, fmt.Errorf("bedrock: read endpoint %s: %w", in.ModelID, err)
	}
	if !found {
		return &DeleteEndpointOutput{}, nil
	}
	if rec.State != StateReady && rec.State != StateDraining {
		return nil, awserrors.Errorf(awserrors.ErrorModelNotReadyException,
			"bedrock: endpoint for %s cannot be deleted (state=%s)", in.ModelID, rec.State)
	}
	if rec.State == StateReady {
		if err := validateTransition(rec.State, StateDraining); err != nil {
			return nil, err
		}
		rec.State = StateDraining
		rec.Generation++
		if err := updateJSON(ctx, kv, key, rev, rec); err != nil {
			return nil, fmt.Errorf("bedrock: mark endpoint %s draining: %w", in.ModelID, err)
		}
	}

	if err := TerminateServingVM(ctx, s.deps.Launch, rec); err != nil {
		return nil, fmt.Errorf("bedrock: delete endpoint %s: %w", in.ModelID, err)
	}
	if err := validateTransition(StateDraining, StateAbsent); err != nil {
		return nil, err
	}
	if err := deleteJSON(ctx, kv, key); err != nil {
		return nil, fmt.Errorf("bedrock: remove endpoint %s record: %w", in.ModelID, err)
	}
	return &DeleteEndpointOutput{}, nil
}

// getFullJSON is getJSONRevision with the found flag surfaced alongside the
// value and revision, for callers (Delete) that branch on all three.
func getFullJSON(ctx context.Context, kv jetstream.KeyValue, key string) (EndpointRecord, uint64, bool, error) {
	var rec EndpointRecord
	rev, found, err := getJSONRevision(ctx, kv, key, &rec)
	return rec, rev, found, err
}

// keyMutex hands out a per-key *sync.Mutex, creating it on first use. Entries
// are never removed: the key space is the model catalog, which is small and
// static, so the map cannot grow unbounded.
type keyMutex struct {
	mu    sync.Mutex
	locks map[string]*sync.Mutex
}

// lock blocks until key's mutex is held and returns the matching unlock func.
func (k *keyMutex) lock(key string) func() {
	k.mu.Lock()
	m, ok := k.locks[key]
	if !ok {
		m = &sync.Mutex{}
		if k.locks == nil {
			k.locks = make(map[string]*sync.Mutex)
		}
		k.locks[key] = m
	}
	k.mu.Unlock()

	m.Lock()
	return m.Unlock
}
