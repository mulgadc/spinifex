package admin

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strconv"
	"time"

	clusterv1 "github.com/mulgadc/spinifex/contracts/cluster/v1"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	natsmsg "github.com/mulgadc/spinifex/spinifex/foundation/messaging/nats"
	"github.com/mulgadc/spinifex/spinifex/foundation/state/kvstore"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
)

// GetNodeFactsOutput is the response for GetNodeFacts.
type GetNodeFactsOutput struct {
	Nodes []clusterv1.NodeFacts `json:"nodes"`
}

// nodeFactsTimeout allows for a node rebuilding its facts, which reads every NIC.
const nodeFactsTimeout = 7 * time.Second

// GetNodeFacts gathers every node's hardware and network facts.
func GetNodeFacts(ctx context.Context, nc *nats.Conn, expectedNodes int) (*GetNodeFactsOutput, error) {
	frames, _, err := natsmsg.Gather(ctx, nc, clusterv1.NodeFactsSubject, []byte("{}"),
		natsmsg.GatherOpts{Timeout: nodeFactsTimeout, ExpectedNodes: expectedNodes})
	if err != nil {
		return nil, err
	}
	out := &GetNodeFactsOutput{Nodes: make([]clusterv1.NodeFacts, 0, len(frames))}
	for _, frame := range frames {
		var f clusterv1.NodeFacts
		if json.Unmarshal(frame.Data, &f) == nil && f.Node != "" {
			out.Nodes = append(out.Nodes, f)
		}
	}
	slices.SortFunc(out.Nodes, func(a, b clusterv1.NodeFacts) int {
		switch {
		case a.Node < b.Node:
			return -1
		case a.Node > b.Node:
			return 1
		}
		return 0
	})
	return out, nil
}

// Network test states.
const (
	NetworkTestRunning  = "running"
	NetworkTestComplete = "complete"
	NetworkTestFailed   = "failed"
	NetworkTestPending  = "pending"
)

// Bounds on one network test, so an admin cannot tie the cluster's links up.
const (
	defaultNetworkTestSeconds = 10
	defaultNetworkTestStreams = 4
	maxNetworkTestRuns        = 24
	maxNetworkTestListed      = 20
	networkTestRunSlack       = 20 * time.Second
	networkTestServeTimeout   = 5 * time.Second
)

// NetworkTest is one admin-requested test: every ordered pair of nodes on a
// plane, run one after another so no two share a link.
type NetworkTest struct {
	TestID          string              `json:"testId"`
	Plane           string              `json:"plane"`
	Status          string              `json:"status"`
	DurationSeconds int                 `json:"durationSeconds"`
	Streams         int                 `json:"streams"`
	StartedAt       time.Time           `json:"startedAt"`
	CompletedAt     *time.Time          `json:"completedAt,omitempty"`
	Error           string              `json:"error,omitempty"`
	Runs            []NetworkTestResult `json:"runs"`
}

// NetworkTestResult is one direction between two nodes.
type NetworkTestResult struct {
	clusterv1.NetTestRunResponse

	From   string `json:"from"`
	To     string `json:"to"`
	Status string `json:"status"`
}

// NetworkTestStore holds tests and the one cluster-wide claim on running one.
type NetworkTestStore interface {
	// Claim takes the cluster-wide slot for testID until the given time, and
	// reports ErrNetworkTestBusy while another unexpired claim holds it.
	Claim(ctx context.Context, testID string, until time.Time) error
	Release(ctx context.Context, testID string) error
	Put(ctx context.Context, t *NetworkTest) error
	Get(ctx context.Context, testID string) (*NetworkTest, error)
	List(ctx context.Context) ([]NetworkTest, error)
}

// ErrNetworkTestBusy is a claim refused because another test is running.
var ErrNetworkTestBusy = errors.New("a network test is already running")

const (
	networkTestBucket   = "spinifex-network-tests"
	networkTestTTL      = 7 * 24 * time.Hour
	networkTestClaimKey = "active"
	networkTestPrefix   = "test."
)

type networkTestClaim struct {
	TestID string    `json:"testId"`
	Until  time.Time `json:"until"`
}

// KVNetworkTests is the NetworkTestStore over a JetStream KV bucket, shared
// by every gateway so a test started through one is visible through all.
type KVNetworkTests struct {
	tests  *kvstore.Store[NetworkTest]
	claims *kvstore.Store[networkTestClaim]
}

var _ NetworkTestStore = (*KVNetworkTests)(nil)

// NewKVNetworkTests constructs the store over the cluster's JetStream client.
func NewKVNetworkTests(js jetstream.JetStream) *KVNetworkTests {
	b := kvstore.NewBucket(js, kvstore.Config{
		Name:    networkTestBucket,
		History: 1,
		TTL:     networkTestTTL,
		Missing: "network tests: no JetStream client configured",
	})
	return &KVNetworkTests{tests: kvstore.On[NetworkTest](b), claims: kvstore.On[networkTestClaim](b)}
}

// Claim is a create-only write; an expired claim, left by a gateway that died
// mid-test, is replaced under its revision so two takers cannot both win.
func (s *KVNetworkTests) Claim(ctx context.Context, testID string, until time.Time) error {
	claim := &networkTestClaim{TestID: testID, Until: until}
	_, err := s.claims.Create(ctx, networkTestClaimKey, claim)
	if !errors.Is(err, kvstore.ErrExists) {
		return err
	}
	held, rev, err := s.claims.Get(ctx, networkTestClaimKey)
	if err != nil {
		return err
	}
	if time.Now().Before(held.Until) {
		return ErrNetworkTestBusy
	}
	if _, err := s.claims.CompareAndSet(ctx, networkTestClaimKey, claim, rev); err != nil {
		if errors.Is(err, kvstore.ErrConflict) {
			return ErrNetworkTestBusy
		}
		return err
	}
	return nil
}

// Release drops the claim only if testID still holds it.
func (s *KVNetworkTests) Release(ctx context.Context, testID string) error {
	held, rev, err := s.claims.Get(ctx, networkTestClaimKey)
	if errors.Is(err, kvstore.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if held.TestID != testID {
		return nil
	}
	return s.claims.CompareAndDelete(ctx, networkTestClaimKey, rev)
}

func (s *KVNetworkTests) Put(ctx context.Context, t *NetworkTest) error {
	return s.tests.Set(ctx, networkTestPrefix+t.TestID, t)
}

func (s *KVNetworkTests) Get(ctx context.Context, testID string) (*NetworkTest, error) {
	t, _, err := s.tests.Get(ctx, networkTestPrefix+testID)
	return t, err
}

func (s *KVNetworkTests) List(ctx context.Context) ([]NetworkTest, error) {
	return s.tests.List(ctx, networkTestPrefix)
}

// StartNetworkTest validates the request, claims the cluster-wide slot,
// records the test and returns at once; the runs converge in the background.
func (d Deps) StartNetworkTest(ctx context.Context, args map[string]string) (*NetworkTest, error) {
	if d.NetworkTests == nil || d.NATS == nil {
		return nil, errors.New(awserrors.ErrorServerInternal)
	}
	plane := args["Plane"]
	if plane != clusterv1.PlaneWAN && plane != clusterv1.PlaneLAN && plane != clusterv1.PlaneVPC {
		return nil, awserrors.Errorf(awserrors.ErrorInvalidParameterValue, "Plane must be wan, lan or vpc")
	}
	duration, err := intArg(args, "DurationSeconds", defaultNetworkTestSeconds, minNetTestSeconds, maxNetTestSeconds)
	if err != nil {
		return nil, err
	}
	streams, err := intArg(args, "Streams", defaultNetworkTestStreams, 1, maxNetTestStreams)
	if err != nil {
		return nil, err
	}
	runs, err := networkTestPairs(d.NodeIDs, args["Source"], args["Target"])
	if err != nil {
		return nil, err
	}

	test := &NetworkTest{
		TestID: newNetworkTestID(), Plane: plane, Status: NetworkTestRunning,
		DurationSeconds: duration, Streams: streams, StartedAt: time.Now().UTC(), Runs: runs,
	}
	perRun := time.Duration(duration)*time.Second + networkTestRunSlack + networkTestServeTimeout
	until := time.Now().Add(time.Duration(len(runs))*perRun + time.Minute)
	switch err := d.NetworkTests.Claim(ctx, test.TestID, until); {
	case errors.Is(err, ErrNetworkTestBusy):
		return nil, awserrors.Errorf(awserrors.ErrorIncorrectState, "%s", err.Error())
	case err != nil:
		slog.ErrorContext(ctx, "StartNetworkTest: claim", "err", err)
		return nil, errors.New(awserrors.ErrorServerInternal)
	}
	if err := d.NetworkTests.Put(ctx, test); err != nil {
		_ = d.NetworkTests.Release(ctx, test.TestID)
		slog.ErrorContext(ctx, "StartNetworkTest: record", "err", err)
		return nil, errors.New(awserrors.ErrorServerInternal)
	}
	slog.InfoContext(ctx, "network test started", "test_id", test.TestID, "plane", plane, "runs", len(runs))

	started := *test
	started.Runs = slices.Clone(test.Runs)
	go d.runNetworkTest(context.WithoutCancel(ctx), test, perRun)
	return &started, nil
}

// These mirror the daemon's limits, which refuse anything outside them.
const (
	minNetTestSeconds = 5
	maxNetTestSeconds = 30
	maxNetTestStreams = 16
)

func intArg(args map[string]string, name string, def, lo, hi int) (int, error) {
	raw := args[name]
	if raw == "" {
		return def, nil
	}
	v, err := strconv.Atoi(raw)
	if err != nil || v < lo || v > hi {
		return 0, awserrors.Errorf(awserrors.ErrorInvalidParameterValue, "%s must be between %d and %d", name, lo, hi)
	}
	return v, nil
}

// networkTestPairs is both directions between source and target, or with no
// target every ordered pair of nodes, the source's first.
func networkTestPairs(nodes []string, source, target string) ([]NetworkTestResult, error) {
	known := func(n string) bool { return slices.Contains(nodes, n) }
	if source != "" && !known(source) {
		return nil, awserrors.Errorf(awserrors.ErrorInvalidParameterValue, "unknown Source node")
	}
	if target != "" && !known(target) {
		return nil, awserrors.Errorf(awserrors.ErrorInvalidParameterValue, "unknown Target node")
	}
	var runs []NetworkTestResult
	add := func(from, to string) {
		runs = append(runs, NetworkTestResult{From: from, To: to, Status: NetworkTestPending})
	}
	switch {
	case source != "" && target != "":
		if source == target {
			return nil, awserrors.Errorf(awserrors.ErrorInvalidParameterCombination, "Source and Target must differ")
		}
		add(source, target)
		add(target, source)
	case target != "":
		return nil, awserrors.Errorf(awserrors.ErrorMissingParameter, "Target needs a Source")
	default:
		order := slices.Clone(nodes)
		slices.Sort(order)
		if source != "" {
			order = append([]string{source}, slices.DeleteFunc(order, func(n string) bool { return n == source })...)
		}
		for _, from := range order {
			for _, to := range order {
				if from != to {
					add(from, to)
				}
			}
		}
	}
	if len(runs) == 0 {
		return nil, awserrors.Errorf(awserrors.ErrorInvalidParameterValue, "a network test needs at least two nodes")
	}
	if len(runs) > maxNetworkTestRuns {
		return nil, awserrors.Errorf(awserrors.ErrorInvalidParameterValue, "too many nodes for a full mesh; name a Source and Target")
	}
	return runs, nil
}

func newNetworkTestID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return "nt-" + hex.EncodeToString(b)
}

// runNetworkTest drives each run in turn and writes the record after each,
// so a poller sees results as they land.
func (d Deps) runNetworkTest(parent context.Context, test *NetworkTest, perRun time.Duration) {
	ctx, cancel := context.WithTimeout(parent, time.Duration(len(test.Runs))*perRun+time.Minute)
	defer cancel()
	defer func() {
		if err := d.NetworkTests.Release(parent, test.TestID); err != nil {
			slog.Warn("network test: release claim", "test_id", test.TestID, "err", err)
		}
	}()

	failed := 0
	for i := range test.Runs {
		run := &test.Runs[i]
		run.Status = NetworkTestRunning
		d.saveNetworkTest(ctx, test)
		res, err := d.runNetworkTestPair(ctx, test, run.From, run.To, perRun)
		run.NetTestRunResponse = res
		if err != nil {
			run.Status, run.Error = NetworkTestFailed, err.Error()
			failed++
		} else {
			run.Status = NetworkTestComplete
		}
	}
	now := time.Now().UTC()
	test.CompletedAt = &now
	test.Status = NetworkTestComplete
	if failed == len(test.Runs) {
		test.Status, test.Error = NetworkTestFailed, "every run failed"
	}
	// The run deadline may have passed, and the final status must still land.
	d.saveNetworkTest(parent, test)
	slog.Info("network test finished", "test_id", test.TestID, "status", test.Status, "failed_runs", failed)
}

func (d Deps) saveNetworkTest(ctx context.Context, test *NetworkTest) {
	if err := d.NetworkTests.Put(ctx, test); err != nil {
		slog.Warn("network test: write record", "test_id", test.TestID, "err", err)
	}
}

func (d Deps) runNetworkTestPair(ctx context.Context, test *NetworkTest, from, to string, perRun time.Duration) (clusterv1.NetTestRunResponse, error) {
	var res clusterv1.NetTestRunResponse
	serveReq, err := json.Marshal(clusterv1.NetTestServeRequest{Plane: test.Plane, DurationSeconds: test.DurationSeconds})
	if err != nil {
		return res, fmt.Errorf("encode serve request: %w", err)
	}
	sctx, scancel := context.WithTimeout(ctx, networkTestServeTimeout)
	defer scancel()
	msg, err := d.NATS.RequestWithContext(sctx, clusterv1.NetTestServeSubject(to), serveReq)
	if err != nil {
		return res, fmt.Errorf("%s did not start its server: %w", to, err)
	}
	var served clusterv1.NetTestServeResponse
	if err := json.Unmarshal(msg.Data, &served); err != nil {
		return res, fmt.Errorf("%s: malformed serve reply", to)
	}
	if served.Error != "" {
		return res, fmt.Errorf("%s: %s", to, served.Error)
	}

	runReq, err := json.Marshal(clusterv1.NetTestRunRequest{
		Plane: test.Plane, Address: served.Address, Port: served.Port,
		DurationSeconds: test.DurationSeconds, Streams: test.Streams,
	})
	if err != nil {
		return res, fmt.Errorf("encode run request: %w", err)
	}
	rctx, cancel := context.WithTimeout(ctx, perRun)
	defer cancel()
	msg, err = d.NATS.RequestWithContext(rctx, clusterv1.NetTestRunSubject(from), runReq)
	if err != nil {
		return res, fmt.Errorf("%s did not report a result: %w", from, err)
	}
	if err := json.Unmarshal(msg.Data, &res); err != nil {
		return res, fmt.Errorf("%s: malformed result", from)
	}
	if res.Error != "" {
		return res, errors.New(res.Error)
	}
	return res, nil
}

// DescribeNetworkTestsOutput is the response for DescribeNetworkTests.
type DescribeNetworkTestsOutput struct {
	Tests []NetworkTest `json:"tests"`
}

// DescribeNetworkTests returns one test by ID, or the most recent ones.
func (d Deps) DescribeNetworkTests(ctx context.Context, args map[string]string) (*DescribeNetworkTestsOutput, error) {
	if d.NetworkTests == nil {
		return nil, errors.New(awserrors.ErrorServerInternal)
	}
	if id := args["TestId"]; id != "" {
		t, err := d.NetworkTests.Get(ctx, id)
		if errors.Is(err, kvstore.ErrNotFound) {
			return nil, awserrors.Errorf(awserrors.ErrorResourceNotFound, "no network test %s", id)
		}
		if err != nil {
			return nil, err
		}
		return &DescribeNetworkTestsOutput{Tests: []NetworkTest{*t}}, nil
	}
	tests, err := d.NetworkTests.List(ctx)
	if err != nil {
		return nil, err
	}
	slices.SortFunc(tests, func(a, b NetworkTest) int { return b.StartedAt.Compare(a.StartedAt) })
	if len(tests) > maxNetworkTestListed {
		tests = tests[:maxNetworkTestListed]
	}
	if tests == nil {
		tests = []NetworkTest{}
	}
	return &DescribeNetworkTestsOutput{Tests: tests}, nil
}
