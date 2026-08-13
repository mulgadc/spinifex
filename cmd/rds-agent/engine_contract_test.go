package main

//test:in-package — the agent is a main package, which has no external test
// package to import it from, and these cases are built on the unexported
// engine interface and each implementation's own constructor.

import (
	"context"
	"errors"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	handlers_rds "github.com/mulgadc/spinifex/spinifex/handlers/rds"
)

// A probe the test drives directly, in place of whichever binaries the engine's
// own state function would have run. Each implementation's state function is
// covered on its own; what these cases exercise is what the engine does with the
// answer.
func drivenProbe(state *engineState) *engineProbe {
	return newEngineProbe(0, func(context.Context, int64) (engineState, string) {
		return *state, "driven by the test"
	})
}

// One engine behind the interfaces the heartbeat, the commander and the rollback
// guard hold, plus the few facts a shared case needs to say what it means.
type engineHarness struct {
	engine  engine
	runner  *recordingRunner
	params  parameterStore
	service string
	// The state the probe reports, moved by the test rather than by the engine.
	state *engineState
	// Points the next quiesce at a session the test can inspect, and returns the
	// commands those sessions were opened with.
	useSession func(*fakeSession) *[]command
	// A setting this engine's catalog really carries, so a shared case installs
	// something the implementation would genuinely be handed.
	parameter handlers_rds.Parameter
	// What the engine's own include directive globs. Neither the rollback copy
	// nor the serving copy may end in it.
	includeSuffix string
}

func engineHarnesses() []struct {
	name  string
	build func(*testing.T) *engineHarness
} {
	return []struct {
		name  string
		build func(*testing.T) *engineHarness
	}{
		{name: enginePostgres, build: buildPostgresHarness},
		{name: engineMariaDB, build: buildMariaDBHarness},
	}
}

func forEachEngine(t *testing.T, run func(*testing.T, *engineHarness)) {
	t.Helper()
	for _, fixture := range engineHarnesses() {
		t.Run(fixture.name, func(t *testing.T) { run(t, fixture.build(t)) })
	}
}

func buildPostgresHarness(t *testing.T) *engineHarness {
	t.Helper()
	runner := &recordingRunner{}
	e := newTestEngine(t, runner.run)
	state := engineServing
	e.probe = drivenProbe(&state)

	return &engineHarness{
		engine: e, runner: runner, params: e.params, service: e.service, state: &state,
		useSession: func(session *fakeSession) *[]command {
			started := &[]command{}
			e.startSess = func(_ context.Context, c command) (engineSession, error) {
				*started = append(*started, c)
				return session, nil
			}
			return started
		},
		parameter:     handlers_rds.Parameter{Name: "work_mem", Value: "4096"},
		includeSuffix: ".conf",
	}
}

func buildMariaDBHarness(t *testing.T) *engineHarness {
	t.Helper()
	runner := &recordingRunner{}
	e := newTestMariaDBEngine(t, runner.run)
	state := engineServing
	e.probe = drivenProbe(&state)

	return &engineHarness{
		engine: e, runner: runner, params: e.params, service: e.service, state: &state,
		useSession: func(session *fakeSession) *[]command {
			started := &[]command{}
			e.startSess = func(_ context.Context, c command) (engineSession, error) {
				*started = append(*started, c)
				return session, nil
			}
			return started
		},
		parameter:     handlers_rds.Parameter{Name: "max_connections", Value: "200"},
		includeSuffix: ".cnf",
	}
}

// The parameters land where rds-init installs them, so the next boot overwrites
// the file rather than shadowing it with a second copy.
func TestEngineContract_ApplyParametersInstallsWhereRdsInitDoes(t *testing.T) {
	forEachEngine(t, func(t *testing.T, h *engineHarness) {
		if _, err := h.engine.ApplyParameters(t.Context(), []handlers_rds.Parameter{h.parameter}); err != nil {
			t.Fatalf("ApplyParameters: %v", err)
		}

		installed, err := os.ReadFile(h.params.installedPath())
		if err != nil {
			t.Fatalf("read the installed parameters: %v", err)
		}
		if !strings.Contains(string(installed), h.parameter.Name+" = '"+h.parameter.Value+"'") {
			t.Errorf("installed parameters = %q, want the resolved value", installed)
		}
		if !strings.HasSuffix(h.params.installedPath(), h.includeSuffix) {
			t.Errorf("the installed set at %s is not a file the engine includes", h.params.installedPath())
		}
		// Comparison material, not a second set of settings: an include glob that
		// read either of these would apply a withdrawn configuration.
		copies := []string{h.params.lastGoodPath()}
		if h.params.serving != "" {
			copies = append(copies, h.params.servingPath())
		}
		for _, path := range copies {
			if strings.HasSuffix(path, h.includeSuffix) {
				t.Errorf("the copy at %s would be included by the engine", path)
			}
		}
	})
}

// An engine still starting or replaying has not finished adopting the set it is
// on, so replacing that set underneath it would be installing over a moving
// target.
func TestEngineContract_ApplyParametersRefusesWhileTheEngineIsRecovering(t *testing.T) {
	forEachEngine(t, func(t *testing.T, h *engineHarness) {
		*h.state = engineRecovering

		if _, err := h.engine.ApplyParameters(t.Context(), []handlers_rds.Parameter{h.parameter}); err == nil {
			t.Fatal("ApplyParameters succeeded against an engine that was still coming up")
		}
		if _, err := os.Stat(h.params.installedPath()); !os.IsNotExist(err) {
			t.Errorf("the refused set was installed anyway (stat err = %v)", err)
		}
	})
}

// The boot-time half of the parameter safety net: the last set the engine
// accepted goes back only when the engine is genuinely down, and only once.
func TestEngineContract_RestoreLastKnownGoodOnlyRollsBackADownEngine(t *testing.T) {
	forEachEngine(t, func(t *testing.T, h *engineHarness) {
		accepted := h.parameter
		if _, err := h.engine.ApplyParameters(t.Context(), []handlers_rds.Parameter{accepted}); err != nil {
			t.Fatalf("ApplyParameters: %v", err)
		}
		if err := h.engine.RecordServingParameters(t.Context()); err != nil {
			t.Fatalf("RecordServingParameters: %v", err)
		}
		if _, err := h.engine.ApplyParameters(t.Context(),
			[]handlers_rds.Parameter{{Name: accepted.Name, Value: accepted.Value + "0"}}); err != nil {
			t.Fatalf("second ApplyParameters: %v", err)
		}

		// A guard whose deadline expired while an apply brought the engine back must
		// not reverse that repair.
		restored, err := h.engine.RestoreLastKnownGoodParameters(t.Context())
		if err != nil {
			t.Fatalf("RestoreLastKnownGoodParameters against a serving engine: %v", err)
		}
		if restored {
			t.Fatal("rolled the parameters back while the engine was serving")
		}

		*h.state = engineAbsent
		restored, err = h.engine.RestoreLastKnownGoodParameters(t.Context())
		if err != nil {
			t.Fatalf("RestoreLastKnownGoodParameters: %v", err)
		}
		if !restored {
			t.Fatal("did not roll back to a set that differs from the last accepted one")
		}
		installed, err := os.ReadFile(h.params.installedPath())
		if err != nil {
			t.Fatalf("read the installed parameters: %v", err)
		}
		if !strings.Contains(string(installed), accepted.Name+" = '"+accepted.Value+"'") {
			t.Errorf("installed = %q, want the last accepted set", installed)
		}

		// Repeating a rollback would only churn a cluster that is failing for
		// another reason.
		restored, err = h.engine.RestoreLastKnownGoodParameters(t.Context())
		if err != nil {
			t.Fatalf("RestoreLastKnownGoodParameters: %v", err)
		}
		if restored {
			t.Error("rolled back again when the installed set already matched")
		}
	})
}

// Through the service manager rather than the engine's own control tool, so the
// supervisor records the engine as stopped and does not restart it underneath a
// VM that is going down.
func TestEngineContract_StopGoesThroughTheServiceManager(t *testing.T) {
	forEachEngine(t, func(t *testing.T, h *engineHarness) {
		if err := h.engine.Stop(t.Context()); err != nil {
			t.Fatalf("Stop: %v", err)
		}
		if len(h.runner.calls) != 1 {
			t.Fatalf("ran %d commands, want the service stop", len(h.runner.calls))
		}
		call := h.runner.calls[0]
		if !slices.Equal(call.Args, []string{h.service, "stop"}) {
			t.Errorf("stop ran %s %v, want the service manager", call.Name, call.Args)
		}
	})
}

// One backup at a time, released by Unquiesce or by its own deadline, whichever
// comes first. The deadline is what keeps a control plane that dies mid-snapshot
// from leaving the engine held.
func TestEngineContract_QuiesceHoldsUntilReleased(t *testing.T) {
	forEachEngine(t, func(t *testing.T, h *engineHarness) {
		session := &fakeSession{}
		started := h.useSession(session)

		if err := h.engine.Quiesce(t.Context(), "orders-db-pre-upgrade", time.Minute); err != nil {
			t.Fatalf("Quiesce: %v", err)
		}
		if len(*started) != 1 {
			t.Fatalf("started %d sessions, want 1", len(*started))
		}
		if session.closes() != 0 {
			t.Fatal("the session was closed while the backup was meant to be held")
		}

		err := h.engine.Quiesce(t.Context(), "second", time.Minute)
		if err == nil {
			t.Fatal("a second quiesce was accepted while the first was held")
		}
		if !strings.Contains(err.Error(), "orders-db-pre-upgrade") {
			t.Errorf("error %q does not name the backup already holding the engine", err)
		}
		if len(*started) != 1 {
			t.Errorf("started %d sessions, want the second refused before opening one", len(*started))
		}

		if err := h.engine.Unquiesce(t.Context()); err != nil {
			t.Fatalf("Unquiesce: %v", err)
		}
		if session.closes() != 1 {
			t.Errorf("closed %d times, want the session ended exactly once", session.closes())
		}
	})
}

// A hold that expired mid-snapshot means the snapshot was not taken against a
// held checkpoint, so the release reports it rather than succeeding silently.
func TestEngineContract_QuiesceReleasesItselfOnItsDeadline(t *testing.T) {
	forEachEngine(t, func(t *testing.T, h *engineHarness) {
		session := &fakeSession{}
		h.useSession(session)

		if err := h.engine.Quiesce(t.Context(), "orders-db-pre-upgrade", 10*time.Millisecond); err != nil {
			t.Fatalf("Quiesce: %v", err)
		}
		deadline := time.Now().Add(2 * time.Second)
		for session.closes() == 0 && time.Now().Before(deadline) {
			time.Sleep(time.Millisecond)
		}
		if session.closes() != 1 {
			t.Fatal("the backup session outlived its deadline")
		}

		err := h.engine.Unquiesce(t.Context())
		if err == nil {
			t.Fatal("Unquiesce succeeded after the hold expired")
		}
		if !strings.Contains(err.Error(), "expired") {
			t.Errorf("error %q does not say the hold had expired", err)
		}
	})
}

// A failed start leaves nothing held, so a retry is not refused by a hold that
// never took effect.
func TestEngineContract_QuiesceEndsTheSessionWhenTheBackupWillNotStart(t *testing.T) {
	forEachEngine(t, func(t *testing.T, h *engineHarness) {
		session := &fakeSession{execErr: errors.New("the engine refused the backup")}
		h.useSession(session)

		if err := h.engine.Quiesce(t.Context(), "orders-db-pre-upgrade", time.Minute); err == nil {
			t.Fatal("Quiesce succeeded against an engine that refused the backup")
		}
		if session.closes() != 1 {
			t.Errorf("closed %d times, want the failed session ended", session.closes())
		}
		if err := h.engine.Unquiesce(t.Context()); err == nil {
			t.Error("a failed quiesce left a hold behind")
		}
	})
}

// Every command the control plane can issue is served by both implementations,
// so a directive cannot be answered as unsupported on one engine and honoured on
// the other.
func TestEngineContract_ServesEveryRegisteredCommand(t *testing.T) {
	forEachEngine(t, func(t *testing.T, h *engineHarness) {
		registry := newCommandRegistry(h.engine, &fakeStorage{})
		for _, name := range []string{
			handlers_rds.CommandSetPassword, handlers_rds.CommandApplyParams, handlers_rds.CommandStopEngine,
			handlers_rds.CommandGrowFilesystem, handlers_rds.CommandQuiesce, handlers_rds.CommandUnquiesce,
		} {
			if _, ok := registry[name]; !ok {
				t.Errorf("command %q has no handler", name)
			}
		}
	})
}
