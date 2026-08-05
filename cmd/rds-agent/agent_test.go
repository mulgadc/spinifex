package main

import (
	"context"
	"errors"
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
	return f.bootstrapOut, nil
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

func TestBootstrap_RetriesHandoffWithoutRefetchingConsumedPassword(t *testing.T) {
	cp := newFakeControlPlane()
	password := "one-shot-secret"
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
