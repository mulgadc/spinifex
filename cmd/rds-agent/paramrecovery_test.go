package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	handlers_rds "github.com/mulgadc/spinifex/spinifex/handlers/rds"
)

// A runner that fails only the offline config check, so the check's own
// rejection is separable from a reload or a psql failure.
type checkFailingRunner struct {
	recordingRunner

	checkErr error
}

func (r *checkFailingRunner) run(ctx context.Context, c command) (string, error) {
	if slices.Contains(c.Args, "-C") {
		r.calls = append(r.calls, c)
		return "", r.checkErr
	}
	return r.recordingRunner.run(ctx, c)
}

// The failure this closes: a value the engine refuses would otherwise sit on the
// data volume, where it survives every VM replace and turns the next restart
// into a boot loop.
func TestPostgresEngine_ApplyParametersRollsBackAValueTheEngineRejects(t *testing.T) {
	runner := &checkFailingRunner{checkErr: errors.New(`unrecognized configuration parameter "work_men"`)}
	engine := newTestEngine(t, runner.run)

	// A first apply the engine accepted, which is what the rejected one has to
	// roll back to.
	runner.checkErr = nil
	if _, err := engine.ApplyParameters(context.Background(),
		[]handlers_rds.Parameter{{Name: "work_mem", Value: "4096"}}); err != nil {
		t.Fatalf("first ApplyParameters: %v", err)
	}
	runner.checkErr = errors.New(`unrecognized configuration parameter "work_men"`)

	if _, err := engine.ApplyParameters(context.Background(),
		[]handlers_rds.Parameter{{Name: "work_mem", Value: "999999999"}}); err == nil {
		t.Fatal("ApplyParameters succeeded against a config the engine rejected")
	}

	installed, err := os.ReadFile(engine.parametersPath())
	if err != nil {
		t.Fatalf("read the installed parameters: %v", err)
	}
	if !strings.Contains(string(installed), "work_mem = '4096'") {
		t.Errorf("installed parameters = %q, want the previous accepted set restored", installed)
	}
}

// The first apply of an instance's life has nothing to roll back to, so a
// rejection has to leave no include at all rather than an empty one.
func TestPostgresEngine_ApplyParametersWithdrawsTheFirstRejectedSet(t *testing.T) {
	runner := &checkFailingRunner{checkErr: errors.New("configuration file contains errors")}
	engine := newTestEngine(t, runner.run)

	if _, err := engine.ApplyParameters(context.Background(),
		[]handlers_rds.Parameter{{Name: "work_mem", Value: "0"}}); err == nil {
		t.Fatal("ApplyParameters succeeded against a config the engine rejected")
	}
	if _, err := os.Stat(engine.parametersPath()); !os.IsNotExist(err) {
		t.Errorf("the rejected include is still installed (stat err = %v)", err)
	}
}

type pendingAfterReloadRunner struct {
	recordingRunner

	pendingReads int
}

func (r *pendingAfterReloadRunner) run(ctx context.Context, c command) (string, error) {
	if strings.Contains(c.Stdin, "pending_restart") {
		r.pendingReads++
		if r.pendingReads > 1 {
			r.calls = append(r.calls, c)
			return "shared_buffers\n", nil
		}
	}
	return r.recordingRunner.run(ctx, c)
}

// A command can beat the first heartbeat. The apply must preserve the file the
// current postmaster started with before installing a static replacement.
func TestPostgresEngine_ApplyBeforeFirstHeartbeatSeedsLastKnownGood(t *testing.T) {
	runner := &pendingAfterReloadRunner{}
	engine := newTestEngine(t, runner.run)
	first := []byte("shared_buffers = '32768'\n")
	if err := os.WriteFile(engine.parametersPath(), first, 0o600); err != nil {
		t.Fatalf("write serving parameter set: %v", err)
	}

	pending, err := engine.ApplyParameters(t.Context(), []handlers_rds.Parameter{{Name: "shared_buffers", Value: "65536"}})
	if err != nil {
		t.Fatalf("ApplyParameters: %v", err)
	}
	if !slices.Equal(pending, []string{"shared_buffers"}) {
		t.Errorf("pending = %v, want shared_buffers", pending)
	}
	lastGood, err := os.ReadFile(engine.lastGoodPath())
	if err != nil {
		t.Fatalf("read last known good: %v", err)
	}
	if !slices.Equal(lastGood, first) {
		t.Errorf("last known good = %q, want the pre-apply set %q", lastGood, first)
	}
}

type blockingParameterRunner struct {
	recordingRunner

	checkStarted  chan struct{}
	continueCheck chan struct{}
	pendingReads  int
}

func (r *blockingParameterRunner) run(ctx context.Context, c command) (string, error) {
	if slices.Contains(c.Args, "-C") {
		r.calls = append(r.calls, c)
		close(r.checkStarted)
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-r.continueCheck:
			return "", nil
		}
	}
	if strings.Contains(c.Stdin, "pending_restart") {
		r.pendingReads++
		if r.pendingReads > 1 {
			r.calls = append(r.calls, c)
			return "shared_buffers\n", nil
		}
	}
	return r.recordingRunner.run(ctx, c)
}

func TestPostgresEngine_ServingSnapshotWaitsForParameterApply(t *testing.T) {
	runner := &blockingParameterRunner{
		checkStarted:  make(chan struct{}),
		continueCheck: make(chan struct{}),
	}
	engine := newTestEngine(t, runner.run)
	first := []byte("shared_buffers = '32768'\n")
	if err := os.WriteFile(engine.parametersPath(), first, 0o600); err != nil {
		t.Fatalf("write serving parameter set: %v", err)
	}
	if err := os.WriteFile(engine.lastGoodPath(), first, 0o600); err != nil {
		t.Fatalf("write last known good: %v", err)
	}

	applyDone := make(chan error, 1)
	go func() {
		_, err := engine.ApplyParameters(t.Context(), []handlers_rds.Parameter{{Name: "shared_buffers", Value: "65536"}})
		applyDone <- err
	}()
	<-runner.checkStarted

	recordDone := make(chan error, 1)
	go func() {
		recordDone <- engine.RecordServingParameters(t.Context())
	}()
	var earlyRecordErr error
	recordedEarly := false
	select {
	case earlyRecordErr = <-recordDone:
		recordedEarly = true
	case <-time.After(10 * time.Millisecond):
	}
	close(runner.continueCheck)
	if err := <-applyDone; err != nil {
		t.Fatalf("ApplyParameters: %v", err)
	}
	if !recordedEarly {
		earlyRecordErr = <-recordDone
	}
	if earlyRecordErr != nil {
		t.Fatalf("RecordServingParameters: %v", earlyRecordErr)
	}
	if recordedEarly {
		t.Error("serving snapshot completed while the parameter apply held its transaction")
	}

	lastGood, err := os.ReadFile(engine.lastGoodPath())
	if err != nil {
		t.Fatalf("read last known good: %v", err)
	}
	if !slices.Equal(lastGood, first) {
		t.Errorf("last known good = %q, want the set served before the apply %q", lastGood, first)
	}
}

// The rollback target moves only when a restart proves the installed static
// values can serve, not when ApplyParameters merely reloads them.
func TestPostgresEngine_LastKnownGoodTracksTheServingSet(t *testing.T) {
	runner := &recordingRunner{}
	engine := newTestEngine(t, runner.run)

	apply := func(value string) {
		t.Helper()
		if _, err := engine.ApplyParameters(context.Background(),
			[]handlers_rds.Parameter{{Name: "work_mem", Value: value}}); err != nil {
			t.Fatalf("ApplyParameters(%s): %v", value, err)
		}
	}
	assertLastGood := func(value string) {
		t.Helper()
		lastGood, err := os.ReadFile(engine.lastGoodPath())
		if err != nil {
			t.Fatalf("read the last known good parameters: %v", err)
		}
		if !strings.Contains(string(lastGood), "work_mem = '"+value+"'") {
			t.Errorf("last known good = %q, want work_mem %s", lastGood, value)
		}
	}

	apply("4096")
	if err := engine.RecordServingParameters(t.Context()); err != nil {
		t.Fatalf("RecordServingParameters: %v", err)
	}
	apply("8192")
	assertLastGood("4096")

	// Simulate the restart that adopts 8192, then another apply before the next
	// restart. The target remains the set the postmaster actually started with.
	if err := engine.RecordServingParameters(t.Context()); err != nil {
		t.Fatalf("RecordServingParameters after restart: %v", err)
	}
	apply("16384")
	assertLastGood("8192")

	if strings.HasSuffix(engine.lastGoodPath(), ".conf") {
		t.Errorf("the rollback copy at %s would be included by the engine", engine.lastGoodPath())
	}
}

func TestPostgresEngine_RecordServingParametersSkipsPendingRestart(t *testing.T) {
	runner := &recordingRunner{}
	engine := newTestEngine(t, runner.run)
	first := []byte("shared_buffers = '32768'\n")
	if err := os.WriteFile(engine.parametersPath(), first, 0o600); err != nil {
		t.Fatalf("write first parameter set: %v", err)
	}
	if err := engine.RecordServingParameters(t.Context()); err != nil {
		t.Fatalf("record first parameter set: %v", err)
	}

	second := []byte("shared_buffers = '65536'\n")
	if err := os.WriteFile(engine.parametersPath(), second, 0o600); err != nil {
		t.Fatalf("write pending parameter set: %v", err)
	}
	runner.out = "shared_buffers\n"
	if err := engine.RecordServingParameters(t.Context()); err != nil {
		t.Fatalf("record with a pending restart: %v", err)
	}
	lastGood, err := os.ReadFile(engine.lastGoodPath())
	if err != nil {
		t.Fatalf("read last known good: %v", err)
	}
	if !slices.Equal(lastGood, first) {
		t.Errorf("last known good = %q, want the previously served set %q", lastGood, first)
	}

	// Once a restart has adopted the installed set, pending_restart clears and
	// the same healthy-heartbeat path may advance the rollback target.
	runner.out = ""
	if err := engine.RecordServingParameters(t.Context()); err != nil {
		t.Fatalf("record the restarted parameter set: %v", err)
	}
	lastGood, err = os.ReadFile(engine.lastGoodPath())
	if err != nil {
		t.Fatalf("read advanced last known good: %v", err)
	}
	if !slices.Equal(lastGood, second) {
		t.Errorf("last known good = %q, want the restarted set %q", lastGood, second)
	}
}

func TestPostgresEngine_RestoreLastKnownGoodParameters(t *testing.T) {
	runner := &recordingRunner{}
	engine := newTestEngine(t, runner.run)

	// Nothing to restore before anything has ever been applied.
	restored, err := engine.RestoreLastKnownGoodParameters(t.Context())
	if err != nil {
		t.Fatalf("RestoreLastKnownGoodParameters: %v", err)
	}
	if restored {
		t.Error("restored a set with no last known good on disk")
	}

	if _, err := engine.ApplyParameters(context.Background(),
		[]handlers_rds.Parameter{{Name: "work_mem", Value: "4096"}}); err != nil {
		t.Fatalf("ApplyParameters(4096): %v", err)
	}
	if err := engine.RecordServingParameters(t.Context()); err != nil {
		t.Fatalf("RecordServingParameters: %v", err)
	}
	if _, err := engine.ApplyParameters(context.Background(),
		[]handlers_rds.Parameter{{Name: "work_mem", Value: "8192"}}); err != nil {
		t.Fatalf("ApplyParameters(8192): %v", err)
	}

	// A guard whose deadline expired while an apply restarted the engine must
	// recheck health under the parameter lock rather than reverse that repair.
	restored, err = engine.RestoreLastKnownGoodParameters(t.Context())
	if err != nil {
		t.Fatalf("RestoreLastKnownGoodParameters against serving engine: %v", err)
	}
	if restored {
		t.Fatal("restored the old set while the engine was serving")
	}

	engine.probe = stubProbe(t, 2, nil)
	restored, err = engine.RestoreLastKnownGoodParameters(t.Context())
	if err != nil {
		t.Fatalf("RestoreLastKnownGoodParameters: %v", err)
	}
	if !restored {
		t.Fatal("did not restore a set that differs from the last known good")
	}
	installed, err := os.ReadFile(engine.parametersPath())
	if err != nil {
		t.Fatalf("read the installed parameters: %v", err)
	}
	if !strings.Contains(string(installed), "work_mem = '4096'") {
		t.Errorf("installed = %q, want the last accepted set", installed)
	}

	// Idempotent: the second call finds them already equal and does nothing, so a
	// repeated rollback cannot churn a cluster that is failing for another reason.
	restored, err = engine.RestoreLastKnownGoodParameters(t.Context())
	if err != nil {
		t.Fatalf("RestoreLastKnownGoodParameters: %v", err)
	}
	if restored {
		t.Error("restored again when the installed set already matched")
	}
}

// Stands in for the engine in the guard's tests: what matters is whether the
// rollback and the restart were reached at all.
type fakeRecovery struct {
	restored   bool
	restoreErr error
	restarts   int
}

func (f *fakeRecovery) RestoreLastKnownGoodParameters(context.Context) (bool, error) {
	return f.restored, f.restoreErr
}

func (f *fakeRecovery) Restart(context.Context) error {
	f.restarts++
	return nil
}

// A probe whose result the test drives directly, standing in for pg_isready.
func stubProbe(t *testing.T, code int, err error) *engineProbe {
	t.Helper()
	cfg := loadConfig(filepath.Join(t.TempDir(), "absent.env"))
	return newEngineProbe(cfg, func(context.Context, string, ...string) (int, error) {
		return code, err
	})
}

func newTestGuard(engine parameterRecovery, probe *engineProbe) *paramGuard {
	g := newParamGuard(engine, probe, nil)
	// Real budgets are minutes, which is the point of them; the behaviour under
	// test is the decision, not the wait.
	g.after, g.poll = 20*time.Millisecond, time.Millisecond
	return g
}

// pg_isready exit 2 is an engine that is not there at all, which after a
// parameter change is the boot-loop shape this breaks.
func TestParamGuard_RollsBackReportsAndRestartsWhenTheEngineNeverComesUp(t *testing.T) {
	engine := &fakeRecovery{restored: true}
	cp := newFakeControlPlane()
	guard := newParamGuard(engine, stubProbe(t, 2, nil), cp)
	guard.id = identity{DBInstanceIdentifier: "db-1"}
	guard.after, guard.poll = 20*time.Millisecond, time.Millisecond
	guard.Run(t.Context())

	if engine.restarts != 1 {
		t.Errorf("restarts = %d, want the engine restarted on the rolled-back set", engine.restarts)
	}
	states := cp.snapshotStates()
	if len(states) != 1 {
		t.Fatalf("submitted states = %d, want the rollback report", len(states))
	}
	if states[0].health != handlers_rds.EngineHealthUnhealthy || states[0].message != handlers_rds.ParameterRollbackMessage {
		t.Errorf("rollback state = %+v, want the unhealthy rollback report", states[0])
	}
}

func TestParamGuard_DoesNothingWhenTheEngineIsServing(t *testing.T) {
	engine := &fakeRecovery{restored: true}
	newTestGuard(engine, stubProbe(t, 0, nil)).Run(t.Context())

	if engine.restarts != 0 {
		t.Errorf("restarts = %d, want none against a healthy engine", engine.restarts)
	}
}

// pg_isready exit 1 is a postmaster that is up and replaying WAL. It is making
// progress on its own, and restarting it would throw the recovery away.
func TestParamGuard_LeavesARecoveringEngineAlone(t *testing.T) {
	engine := &fakeRecovery{restored: true}
	guard := newTestGuard(engine, stubProbe(t, 1, nil))
	// Bounded so the deadline-resetting loop cannot run forever if it stops
	// resetting; the assertion is that the rollback was never reached.
	ctx, cancel := context.WithTimeout(t.Context(), 200*time.Millisecond)
	defer cancel()
	guard.Run(ctx)

	if engine.restarts != 0 {
		t.Errorf("restarts = %d, want none while the engine is recovering", engine.restarts)
	}
}

// The parameters are only the cause if they differ from the set the engine last
// accepted; restarting on an identical set would churn for nothing.
func TestParamGuard_DoesNotRestartWhenTheParametersAreAlreadyTheAcceptedSet(t *testing.T) {
	engine := &fakeRecovery{restored: false}
	newTestGuard(engine, stubProbe(t, 2, nil)).Run(t.Context())

	if engine.restarts != 0 {
		t.Errorf("restarts = %d, want none when there was nothing to roll back", engine.restarts)
	}
}
