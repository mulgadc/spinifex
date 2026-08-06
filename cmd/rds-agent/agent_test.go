package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mulgadc/spinifex/internal/gwsign"
	"github.com/mulgadc/spinifex/internal/rdsgw"
	gateway_rds "github.com/mulgadc/spinifex/spinifex/gateway/rds"
	handlers_rds "github.com/mulgadc/spinifex/spinifex/handlers/rds"
	"github.com/mulgadc/spinifex/spinifex/utils"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeControlPlane is a scriptable controlPlane. Every call records what the
// agent sent, so a test asserts on the request as well as the reaction.
type fakeControlPlane struct {
	mu sync.Mutex

	registerOut  *handlers_rds.RegisterDBInstanceOutput
	registerErrs []error
	registerReqs []identity

	bootstrapOut  *handlers_rds.GetDBBootstrapConfigOutput
	bootstrapErrs []error
	bootstrapReqs []identity
	// Scripted responses ahead of bootstrapOut, so a test can answer the first
	// fetch differently from the ones the agent retries with.
	bootstrapQueue []*handlers_rds.GetDBBootstrapConfigOutput

	ackErrs []error
	ackReqs []pendingBootstrap

	states []submittedState

	pollReplies [][]handlers_rds.CommandReply
	pollQueue   [][]handlers_rds.Command
	pollErr     error
	polled      chan struct{}
}

type submittedState struct {
	id      identity
	health  handlers_rds.EngineHealth
	message string
}

var _ controlPlane = (*fakeControlPlane)(nil)

func newFakeControlPlane() *fakeControlPlane {
	return &fakeControlPlane{
		registerOut: &handlers_rds.RegisterDBInstanceOutput{
			DBInstanceIdentifier: "db-resolved", HeartbeatIntervalSeconds: 30,
		},
		polled: make(chan struct{}, 64),
	}
}

// nextErr pops the next scripted error, so a test can fail a call n times and
// then let it succeed.
func nextErr(errs *[]error) error {
	if len(*errs) == 0 {
		return nil
	}
	err := (*errs)[0]
	*errs = (*errs)[1:]
	return err
}

func (f *fakeControlPlane) Register(_ context.Context, id identity) (*handlers_rds.RegisterDBInstanceOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.registerReqs = append(f.registerReqs, id)
	if err := nextErr(&f.registerErrs); err != nil {
		return nil, err
	}
	return f.registerOut, nil
}

func (f *fakeControlPlane) SubmitState(_ context.Context, id identity, health handlers_rds.EngineHealth, message string) (*handlers_rds.SubmitDBStateChangeOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.states = append(f.states, submittedState{id: id, health: health, message: message})
	// Zero means "keep the cadence you have", so a test's fast interval is not
	// reset by every beat.
	return &handlers_rds.SubmitDBStateChangeOutput{Acknowledged: true}, nil
}

func (f *fakeControlPlane) GetBootstrapConfig(_ context.Context, id identity) (*handlers_rds.GetDBBootstrapConfigOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.bootstrapReqs = append(f.bootstrapReqs, id)
	if err := nextErr(&f.bootstrapErrs); err != nil {
		return nil, err
	}
	if len(f.bootstrapQueue) > 0 {
		out := f.bootstrapQueue[0]
		f.bootstrapQueue = f.bootstrapQueue[1:]
		return out, nil
	}
	return f.bootstrapOut, nil
}

func (f *fakeControlPlane) AcknowledgeBootstrap(_ context.Context, _ identity, pending *pendingBootstrap) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ackReqs = append(f.ackReqs, *pending)
	return nextErr(&f.ackErrs)
}

func (f *fakeControlPlane) PollCommands(ctx context.Context, _ identity, replies []handlers_rds.CommandReply, _ time.Duration) ([]handlers_rds.Command, error) {
	f.mu.Lock()
	f.pollReplies = append(f.pollReplies, slices.Clone(replies))
	err := f.pollErr
	var out []handlers_rds.Command
	if len(f.pollQueue) > 0 {
		out, f.pollQueue = f.pollQueue[0], f.pollQueue[1:]
	}
	f.mu.Unlock()

	select {
	case f.polled <- struct{}{}:
	default:
	}
	if err != nil {
		return nil, err
	}
	if len(out) > 0 {
		return out, nil
	}
	// An idle window: block until the agent hangs up, as the gateway's long poll
	// does, rather than spinning the caller's loop.
	<-ctx.Done()
	return nil, ctx.Err()
}

func (f *fakeControlPlane) ackRequests() []pendingBootstrap {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.ackReqs)
}

func (f *fakeControlPlane) snapshotStates() []submittedState {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]submittedState(nil), f.states...)
}

func (f *fakeControlPlane) snapshotReplies() [][]handlers_rds.CommandReply {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([][]handlers_rds.CommandReply, len(f.pollReplies))
	for i := range f.pollReplies {
		out[i] = slices.Clone(f.pollReplies[i])
	}
	return out
}

// testConfig points the agent's file output at a temp dir and its probe at a
// name no real binary answers to.
func testConfig(t *testing.T) config {
	t.Helper()
	return config{
		GatewayURL: "https://gw.test",
		HandoffDir: filepath.Join(t.TempDir(), "spinifex-rds"),
		EngineHost: defaultEngineHost,
		EnginePort: defaultEnginePort,
		PGIsReady:  "pg_isready",
		PollWait:   time.Second,
	}
}

// staticProbe is a probeRunner that always reports the same exit code.
func staticProbe(code int) probeRunner {
	return func(context.Context, string, ...string) (int, error) { return code, nil }
}

func bootstrapOutput(password *string) *handlers_rds.GetDBBootstrapConfigOutput {
	mode := handlers_rds.BootstrapModeAttach
	if password != nil {
		mode = handlers_rds.BootstrapModeInitialize
	}
	return &handlers_rds.GetDBBootstrapConfigOutput{
		Mode:                 mode,
		DBInstanceIdentifier: "db-resolved",
		Engine:               "postgres",
		MasterUsername:       "master",
		MasterUserPassword:   password,
		Port:                 5432,
		DataVolumeID:         "vol-data-01",
		DataVolumeSerial:     "voldata01",
		VMGeneration:         1,
		PayloadID:            testPayloadID,
		BootstrapPending:     password != nil,
	}
}

const testPayloadID = "bp-0123456789abcdef"

// Writes the completion receipt rds-init leaves behind, so the acknowledgement
// step finds what a finished initialization would have produced.
func writeReceipt(t *testing.T, a *Agent, payloadID, dbInstanceIdentifier string) {
	t.Helper()
	path := a.receiptPath()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("mkdir receipt dir: %v", err)
	}
	body := fmt.Sprintf("%s=%s\n%s=%s\n", receiptPayload, payloadID, receiptDBID, dbInstanceIdentifier)
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write receipt: %v", err)
	}
}

// runAgent runs the agent until its bootstrap has landed, then cancels. It
// returns once Run has exited so no goroutine outlives the test.
func runAgent(t *testing.T, a *Agent) error {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	done := make(chan error, 1)
	go func() { done <- a.Run(ctx) }()

	waitFor(t, func() bool {
		_, err := os.Stat(filepath.Join(a.cfg.HandoffDir, handoffEnvFile))
		return err == nil
	}, "handoff file to be written")

	cancel()
	select {
	case err := <-done:
		// The cancellation above is this helper's own, so Run reporting it is the
		// expected clean stop rather than a failure, exactly as main treats it.
		if errors.Is(err, context.Canceled) {
			return nil
		}
		return err
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not exit after cancellation")
		return nil
	}
}

// waitFor polls cond until it holds or the test times out.
func waitFor(t *testing.T, cond func() bool, what string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestRun_RegistersThenDeliversBootstrap(t *testing.T) {
	password := "s3cret"
	cp := newFakeControlPlane()
	cp.bootstrapOut = bootstrapOutput(&password)

	cfg := testConfig(t)
	a := newAgent(cfg, cp, newEngineProbe(cfg, staticProbe(0)))
	if err := runAgent(t, a); err != nil {
		t.Fatalf("Run: %v", err)
	}

	if len(cp.registerReqs) != 1 {
		t.Fatalf("registered %d times, want 1", len(cp.registerReqs))
	}
	// The agent had no configured identifier, so the gateway's resolution is
	// what every later call must carry.
	if got := cp.bootstrapReqs[0].DBInstanceIdentifier; got != "db-resolved" {
		t.Errorf("bootstrap sent identifier %q, want the registered db-resolved", got)
	}

	env := readFile(t, filepath.Join(cfg.HandoffDir, handoffEnvFile))
	if !strings.Contains(env, "RDS_MODE='initialize'") || !strings.Contains(env, "RDS_MASTER_PASSWORD='s3cret'") {
		t.Errorf("handoff env = %q, want initialize mode carrying the password", env)
	}
	// The probe follows the port the control plane assigned, not the default.
	if got := a.probe.port.Load(); got != 5432 {
		t.Errorf("probe port = %d, want 5432", got)
	}
}

// A reboot re-fetches in attach mode. The agent must write that payload as it
// stands rather than refusing it or inventing a password.
func TestRun_AttachModeNeedsNoPassword(t *testing.T) {
	cp := newFakeControlPlane()
	cp.bootstrapOut = bootstrapOutput(nil)

	cfg := testConfig(t)
	if err := runAgent(t, newAgent(cfg, cp, newEngineProbe(cfg, staticProbe(0)))); err != nil {
		t.Fatalf("Run: %v", err)
	}

	env := readFile(t, filepath.Join(cfg.HandoffDir, handoffEnvFile))
	if !strings.Contains(env, "RDS_MODE='attach'") {
		t.Errorf("handoff env = %q, want attach mode", env)
	}
	if strings.Contains(env, "RDS_MASTER_PASSWORD") {
		t.Errorf("handoff env = %q, want no password key at all in attach mode", env)
	}
}

// The record may not exist yet when a freshly launched VM first calls, so both
// boot-critical calls have to ride out failures instead of exiting.
func TestRun_RetriesRegisterAndBootstrap(t *testing.T) {
	cp := newFakeControlPlane()
	cp.bootstrapOut = bootstrapOutput(nil)
	cp.registerErrs = []error{errors.New("DBInstanceNotFound")}
	cp.bootstrapErrs = []error{errors.New("ServerInternal")}

	cfg := testConfig(t)
	if err := runAgent(t, newAgent(cfg, cp, newEngineProbe(cfg, staticProbe(0)))); err != nil {
		t.Fatalf("Run: %v", err)
	}

	if len(cp.registerReqs) != 2 {
		t.Errorf("register attempts = %d, want 2 (one failure, one success)", len(cp.registerReqs))
	}
	if len(cp.bootstrapReqs) != 2 {
		t.Errorf("bootstrap attempts = %d, want 2 (one failure, one success)", len(cp.bootstrapReqs))
	}
}

// The fetch mutates nothing, so a handoff that cannot be written is retried
// against the payload already in hand rather than costing a second fetch.
func TestBootstrap_RetriesHandoffWithoutRefetching(t *testing.T) {
	cp := newFakeControlPlane()
	password := "staged-secret"
	cp.bootstrapOut = bootstrapOutput(&password)
	cfg := testConfig(t)
	a := newAgent(cfg, cp, newEngineProbe(cfg, staticProbe(0)))

	writes := 0
	a.handoffWriter = func(_ string, got *handlers_rds.GetDBBootstrapConfigOutput) error {
		writes++
		if got.MasterUserPassword == nil || *got.MasterUserPassword != password {
			t.Fatalf("handoff attempt %d lost the initialize password", writes)
		}
		if writes == 1 {
			return errors.New("temporary tmpfs write error")
		}
		return nil
	}

	if err := a.bootstrap(t.Context()); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	if len(cp.bootstrapReqs) != 1 {
		t.Errorf("bootstrap fetches = %d, want 1", len(cp.bootstrapReqs))
	}
	if writes != 2 {
		t.Errorf("handoff writes = %d, want 2", writes)
	}
	if a.pending == nil || a.pending.payloadID != testPayloadID {
		t.Errorf("pending bootstrap = %+v, want the staged payload %s", a.pending, testPayloadID)
	}
}

// A daemon that predates the encrypted payload answers initialize with nothing
// to initialise with. Writing that handoff would let rds-init run initdb with an
// empty master password, so it is retried until an upgraded node answers.
func TestBootstrap_RetriesInitializeWithNoPassword(t *testing.T) {
	cp := newFakeControlPlane()
	empty := ""
	password := "staged-secret"
	cp.bootstrapQueue = []*handlers_rds.GetDBBootstrapConfigOutput{{
		Mode: handlers_rds.BootstrapModeInitialize, DBInstanceIdentifier: "db-resolved",
		MasterUsername: "master", MasterUserPassword: &empty,
	}}
	cp.bootstrapOut = bootstrapOutput(&password)
	cfg := testConfig(t)
	a := newAgent(cfg, cp, newEngineProbe(cfg, staticProbe(0)))

	var handed []*handlers_rds.GetDBBootstrapConfigOutput
	a.handoffWriter = func(_ string, got *handlers_rds.GetDBBootstrapConfigOutput) error {
		handed = append(handed, got)
		return nil
	}

	if err := a.bootstrap(t.Context()); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	if len(handed) != 1 {
		t.Fatalf("handoff writes = %d, want 1 once a usable payload arrived", len(handed))
	}
	if handed[0].MasterUserPassword == nil || *handed[0].MasterUserPassword != password {
		t.Error("the agent wrote a handoff for a fetch that carried no master password")
	}
}

func TestAcknowledgeBootstrap_WaitsForTheReceiptThenRetiresIt(t *testing.T) {
	cp := newFakeControlPlane()
	cfg := testConfig(t)
	cfg.DataMount = t.TempDir()
	a := newAgent(cfg, cp, newEngineProbe(cfg, staticProbe(0)))
	a.id = identity{DBInstanceIdentifier: "db-resolved"}
	a.pending = &pendingBootstrap{payloadID: testPayloadID, vmGeneration: 1, dataVolumeID: "vol-data-01"}

	// rds-init runs after the agent registers, so the receipt is absent when the
	// acknowledgement step first looks and appears some way into the boot.
	if err := a.checkBootstrapReceipt(a.pending); err == nil {
		t.Fatal("a missing receipt must not read as a completed bootstrap")
	}
	writeReceipt(t, a, testPayloadID, "db-resolved")

	if err := a.acknowledgeBootstrap(t.Context()); err != nil {
		t.Fatalf("acknowledgeBootstrap: %v", err)
	}
	if len(cp.ackRequests()) != 1 {
		t.Fatalf("acknowledgements = %d, want 1", len(cp.ackRequests()))
	}
	if got := cp.ackRequests()[0]; got != *a.pending {
		t.Errorf("acknowledged %+v, want %+v", got, *a.pending)
	}
	if _, err := os.Stat(a.receiptPath()); !os.IsNotExist(err) {
		t.Error("the receipt must be removed once the record holds the audit trail")
	}
}

// The receipt rides along in every snapshot of the data volume, so a restored
// instance's volume carries the source instance's receipt. Acknowledging on one
// would destroy the ciphertext this instance still needs.
func TestAcknowledgeBootstrap_IgnoresAForeignReceipt(t *testing.T) {
	tests := map[string]struct{ payloadID, dbID string }{
		"another payload":  {payloadID: "bp-somebody-else", dbID: "db-resolved"},
		"another instance": {payloadID: testPayloadID, dbID: "db-the-snapshot-source"},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			cp := newFakeControlPlane()
			cfg := testConfig(t)
			cfg.DataMount = t.TempDir()
			a := newAgent(cfg, cp, newEngineProbe(cfg, staticProbe(0)))
			a.id = identity{DBInstanceIdentifier: "db-resolved"}
			a.pending = &pendingBootstrap{payloadID: testPayloadID, vmGeneration: 1}
			writeReceipt(t, a, tc.payloadID, tc.dbID)

			ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
			defer cancel()
			if err := a.acknowledgeBootstrap(ctx); err == nil {
				t.Fatal("acknowledgeBootstrap returned nil, want it still waiting on a real receipt")
			}
			if n := len(cp.ackRequests()); n != 0 {
				t.Errorf("acknowledgements = %d, want 0", n)
			}
		})
	}
}

// An attach boot has nothing staged, so it must not go looking for a receipt the
// datadir will never grow.
func TestAcknowledgeBootstrap_IsANoOpOnAttach(t *testing.T) {
	cp := newFakeControlPlane()
	cfg := testConfig(t)
	cfg.DataMount = t.TempDir()
	a := newAgent(cfg, cp, newEngineProbe(cfg, staticProbe(0)))

	if err := a.acknowledgeBootstrap(t.Context()); err != nil {
		t.Fatalf("acknowledgeBootstrap: %v", err)
	}
	if n := len(cp.ackRequests()); n != 0 {
		t.Errorf("acknowledgements = %d, want 0 when nothing was staged", n)
	}
}

// A configured identifier is asserted on the wire so a mis-provisioned VM is
// rejected by the gateway instead of adopting whatever it resolves to.
func TestRun_SendsConfiguredIdentifier(t *testing.T) {
	cp := newFakeControlPlane()
	cp.bootstrapOut = bootstrapOutput(nil)

	cfg := testConfig(t)
	cfg.DBInstanceIdentifier = "db-configured"
	cfg.EngineVersion = "18.1"
	if err := runAgent(t, newAgent(cfg, cp, newEngineProbe(cfg, staticProbe(0)))); err != nil {
		t.Fatalf("Run: %v", err)
	}

	sent := cp.registerReqs[0]
	if sent.DBInstanceIdentifier != "db-configured" || sent.EngineVersion != "18.1" {
		t.Errorf("register sent %+v, want the configured identifier and engine version", sent)
	}
}

func TestRun_WaitsForDataMountBeforePollingCommands(t *testing.T) {
	cp := newFakeControlPlane()
	cp.bootstrapOut = bootstrapOutput(nil)
	cfg := testConfig(t)
	a := newAgent(cfg, cp, newEngineProbe(cfg, staticProbe(2)))

	waiting := make(chan struct{})
	release := make(chan struct{})
	a.dataMountWaiter = func(ctx context.Context, mountsFile, target string) error {
		if mountsFile != defaultMountsFile || target != defaultDataMount {
			t.Errorf("mount waiter got %q/%q, want configured defaults", mountsFile, target)
		}
		close(waiting)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-release:
			return nil
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- a.Run(ctx) }()

	select {
	case <-waiting:
	case <-ctx.Done():
		t.Fatal("agent did not reach the data-mount wait after writing the handoff")
	}
	if _, err := os.Stat(filepath.Join(cfg.HandoffDir, handoffEnvFile)); err != nil {
		t.Fatalf("handoff was not present before mount wait: %v", err)
	}
	select {
	case <-cp.polled:
		t.Fatal("command poll started before the data volume was mounted")
	case <-time.After(50 * time.Millisecond):
	}

	close(release)
	select {
	case <-cp.polled:
	case <-ctx.Done():
		t.Fatal("command poll did not start after the data mount became ready")
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("Run: %v", err)
	}
}

func TestMountTableContains(t *testing.T) {
	mounts := filepath.Join(t.TempDir(), "mounts")
	require.NoError(t, os.WriteFile(mounts, []byte(
		"/dev/vda1 / ext4 rw 0 0\n/dev/vdb /var/lib/postgresql ext4 rw 0 0\n"+
			"/dev/vdc /path\\040with\\040spaces xfs rw 0 0\nmalformed\n"), 0o600))

	tests := []struct {
		target string
		want   bool
	}{
		{target: "/var/lib/postgresql", want: true},
		{target: "/path with spaces", want: true},
		{target: "/missing", want: false},
	}
	for _, tc := range tests {
		t.Run(tc.target, func(t *testing.T) {
			got, err := mountTableContains(mounts, tc.target)
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}
}

func TestRun_HeartbeatIncludesBootstrapFailure(t *testing.T) {
	cp := newFakeControlPlane()
	cp.bootstrapErrs = []error{errors.New("403 AccessDenied: instance profile is not authorised")}
	cp.registerOut.HeartbeatIntervalSeconds = 0

	cfg := testConfig(t)
	a := newAgent(cfg, cp, newEngineProbe(cfg, staticProbe(2)))
	a.hb.interval = 10 * time.Millisecond

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- a.Run(ctx) }()

	waitFor(t, func() bool {
		for _, state := range cp.snapshotStates() {
			if strings.Contains(state.message, "bootstrap fetch failed") &&
				strings.Contains(state.message, "403 AccessDenied") {
				return true
			}
		}
		return false
	}, "the bootstrap failure to reach a heartbeat")
	cancel()
	<-done
}

func TestRun_HeartbeatsEngineHealth(t *testing.T) {
	cp := newFakeControlPlane()
	cp.bootstrapOut = bootstrapOutput(nil)
	// Declining to set a cadence leaves the agent's own, which the test shortens
	// rather than waiting out the 30s production interval.
	cp.registerOut.HeartbeatIntervalSeconds = 0

	cfg := testConfig(t)
	a := newAgent(cfg, cp, newEngineProbe(cfg, staticProbe(0)))
	a.hb.interval = 10 * time.Millisecond

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- a.Run(ctx) }()

	waitFor(t, func() bool { return len(cp.snapshotStates()) > 0 }, "a heartbeat")
	cancel()
	<-done

	beat := cp.snapshotStates()[0]
	if beat.health != handlers_rds.EngineHealthHealthy {
		t.Errorf("heartbeat health = %q, want healthy", beat.health)
	}
	if beat.id.DBInstanceIdentifier != "db-resolved" {
		t.Errorf("heartbeat identifier = %q, want db-resolved", beat.id.DBInstanceIdentifier)
	}
}

// The cadence is the control plane's to set: it feeds the staleness window the
// recovery reconciler judges an instance against, so the guest adopts it rather
// than keeping its own.
func TestRegister_AdoptsControlPlaneCadence(t *testing.T) {
	cp := newFakeControlPlane()
	cp.bootstrapOut = bootstrapOutput(nil)
	cp.registerOut.HeartbeatIntervalSeconds = 45

	cfg := testConfig(t)
	a := newAgent(cfg, cp, newEngineProbe(cfg, staticProbe(0)))
	if err := runAgent(t, a); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if a.hb.interval != 45*time.Second {
		t.Errorf("heartbeat interval = %v, want the 45s the control plane returned", a.hb.interval)
	}
}

// The agent being up says nothing about the engine. A down engine has to reach
// the control plane as such, or auto-recovery has nothing to act on.
func TestRun_HeartbeatReportsDownEngine(t *testing.T) {
	cp := newFakeControlPlane()
	cp.bootstrapOut = bootstrapOutput(nil)
	cp.registerOut.HeartbeatIntervalSeconds = 0

	cfg := testConfig(t)
	probe := newEngineProbe(cfg, staticProbe(2))
	// The engine has served before, so a silent one now is a failure rather
	// than a boot still in progress.
	probe.seenHealthy = true

	a := newAgent(cfg, cp, probe)
	a.hb.interval = 10 * time.Millisecond

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- a.Run(ctx) }()

	waitFor(t, func() bool { return len(cp.snapshotStates()) > 0 }, "a heartbeat")
	cancel()
	<-done

	beat := cp.snapshotStates()[0]
	if beat.health != handlers_rds.EngineHealthUnhealthy {
		t.Errorf("heartbeat health = %q, want unhealthy while the agent is up", beat.health)
	}
	if beat.message == "" {
		t.Error("unhealthy heartbeat carried no message explaining the probe result")
	}
}

// The mirror the agent decodes poll responses into has to stay aligned with the
// gateway type that produces them; nothing else in the build ties the two.
func TestPollDBCommandsOutput_MatchesGatewayShape(t *testing.T) {
	issued := time.Date(2026, 7, 28, 1, 2, 3, 0, time.UTC)
	// Rendered exactly as the gateway's typed adapter renders it, so a change to
	// either side's tags fails here.
	payload := utils.GenerateIAMXMLPayload("PollDBCommands", &gateway_rds.PollDBCommandsOutput{
		Commands: []handlers_rds.Command{{
			CommandID:  "cmd-1",
			Type:       "reload-parameters",
			Parameters: []handlers_rds.Parameter{{Name: "work_mem", Value: "8MB"}},
			IssuedAt:   &issued,
		}},
	})
	body, err := utils.MarshalToXML(payload)
	if err != nil {
		t.Fatalf("marshal poll result: %v", err)
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/xml")
		_, _ = w.Write(body)
	}))
	defer srv.Close()

	client, err := rdsgw.New(srv.URL, "", gwsign.NewStatic("AKIATEST", "secret"), "us-east-1", time.Second)
	if err != nil {
		t.Fatalf("rdsgw.New: %v", err)
	}

	var got pollDBCommandsOutput
	if err := client.Call(context.Background(), "PollDBCommands", nil, &got); err != nil {
		t.Fatalf("Call: %v", err)
	}
	if len(got.Commands) != 1 {
		t.Fatalf("decoded %d commands, want 1", len(got.Commands))
	}
	cmd := got.Commands[0]
	if cmd.CommandID != "cmd-1" || cmd.Type != "reload-parameters" {
		t.Errorf("command = %+v, want cmd-1/reload-parameters", cmd)
	}
	if len(cmd.Parameters) != 1 || cmd.Parameters[0].Value != "8MB" {
		t.Errorf("Parameters = %+v, want the seeded member", cmd.Parameters)
	}
	if cmd.IssuedAt == nil || !cmd.IssuedAt.Equal(issued) {
		t.Errorf("IssuedAt = %v, want %v", cmd.IssuedAt, issued)
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(body)
}
