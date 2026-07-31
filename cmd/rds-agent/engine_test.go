package main

import (
	"context"
	"errors"
	"os"
	"os/user"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	handlers_rds "github.com/mulgadc/spinifex/spinifex/handlers/rds"
)

// Stands in for the engine in the registry tests, where what matters is that a
// command reaches the right method with the right arguments.
type fakeEngine struct {
	username string
	password string
	params   []handlers_rds.Parameter
	pending  []string
	stopped  bool
	label    string
	hold     time.Duration
	released bool
	err      error
}

var _ engineOps = (*fakeEngine)(nil)

func (f *fakeEngine) SetPassword(_ context.Context, username, password string) error {
	f.username, f.password = username, password
	return f.err
}

func (f *fakeEngine) ApplyParameters(_ context.Context, params []handlers_rds.Parameter) ([]string, error) {
	f.params = params
	return f.pending, f.err
}

func (f *fakeEngine) Stop(context.Context) error {
	f.stopped = true
	return f.err
}

func (f *fakeEngine) Quiesce(_ context.Context, label string, hold time.Duration) error {
	f.label, f.hold = label, hold
	return f.err
}

func (f *fakeEngine) Unquiesce(context.Context) error {
	f.released = true
	return f.err
}

func TestCommandRegistry_SetPasswordCarriesBothParameters(t *testing.T) {
	engine := &fakeEngine{}
	reply := newCommander(nil, newCommandRegistry(engine, &fakeStorage{}), 0).execute(context.Background(), handlers_rds.Command{
		CommandID: "cmd-1",
		Type:      handlers_rds.CommandSetPassword,
		Parameters: []handlers_rds.Parameter{
			{Name: handlers_rds.CommandParamMasterUsername, Value: "postgres"},
			{Name: handlers_rds.CommandParamMasterUserPassword, Value: "n3w-pw"},
		},
	})

	if reply.Status != handlers_rds.CommandStatusSucceeded {
		t.Fatalf("reply = %+v, want succeeded", reply)
	}
	if engine.username != "postgres" || engine.password != "n3w-pw" {
		t.Errorf("engine got %q/%q, want postgres/n3w-pw", engine.username, engine.password)
	}
	// The reply is recorded by the control plane; the new password must not be
	// in it.
	if strings.Contains(reply.Message, "n3w-pw") {
		t.Errorf("reply message %q carries the new password", reply.Message)
	}
}

// CommandReply has no structured payload, so the settings still awaiting a
// restart come back as the message the issuer parses.
func TestCommandRegistry_ApplyParamsReportsPendingRestart(t *testing.T) {
	engine := &fakeEngine{pending: []string{"shared_buffers", "max_connections"}}
	reply := newCommander(nil, newCommandRegistry(engine, &fakeStorage{}), 0).execute(context.Background(), handlers_rds.Command{
		CommandID:  "cmd-2",
		Type:       handlers_rds.CommandApplyParams,
		Parameters: []handlers_rds.Parameter{{Name: "shared_buffers", Value: "256MB"}},
	})

	if reply.Status != handlers_rds.CommandStatusSucceeded {
		t.Fatalf("reply = %+v, want succeeded", reply)
	}
	if reply.Message != "shared_buffers,max_connections" {
		t.Errorf("reply message = %q, want the pending-restart list", reply.Message)
	}
	if len(engine.params) != 1 || engine.params[0].Name != "shared_buffers" {
		t.Errorf("engine got %+v, want the command's parameters", engine.params)
	}
}

func TestCommandRegistry_StopEngineReachesTheEngine(t *testing.T) {
	engine := &fakeEngine{}
	reply := newCommander(nil, newCommandRegistry(engine, &fakeStorage{}), 0).execute(context.Background(),
		handlers_rds.Command{CommandID: "cmd-3", Type: handlers_rds.CommandStopEngine})

	if reply.Status != handlers_rds.CommandStatusSucceeded {
		t.Fatalf("reply = %+v, want succeeded", reply)
	}
	if !engine.stopped {
		t.Error("stop-engine did not reach the engine")
	}
}

func TestCommandRegistry_FailureRepliesFailed(t *testing.T) {
	engine := &fakeEngine{err: errors.New("the engine did not stop")}
	reply := newCommander(nil, newCommandRegistry(engine, &fakeStorage{}), 0).execute(context.Background(),
		handlers_rds.Command{CommandID: "cmd-4", Type: handlers_rds.CommandStopEngine})

	if reply.Status != handlers_rds.CommandStatusFailed {
		t.Fatalf("reply = %+v, want failed", reply)
	}
	if !strings.Contains(reply.Message, "the engine did not stop") {
		t.Errorf("reply message = %q, want the engine's error", reply.Message)
	}
}

// Records what a postgresEngine would have run, so the SQL and the argv are
// asserted without a live cluster.
type recordingRunner struct {
	calls []command
	out   string
	err   error
}

func (r *recordingRunner) run(_ context.Context, c command) (string, error) {
	r.calls = append(r.calls, c)
	return r.out, r.err
}

func newTestEngine(t *testing.T, run commandRunner) *postgresEngine {
	t.Helper()
	cfg := loadConfig(filepath.Join(t.TempDir(), "absent.env"))
	cfg.PGData = t.TempDir()
	if err := os.MkdirAll(filepath.Join(cfg.PGData, "conf.d"), 0o700); err != nil {
		t.Fatalf("create conf.d: %v", err)
	}
	// The guest's postgres user does not exist here, and the chown of the
	// installed parameter file has to resolve to something real.
	current, err := user.Current()
	if err != nil {
		t.Fatalf("resolve the current user: %v", err)
	}
	cfg.PGUser = current.Username
	return newPostgresEngine(cfg, run, nil)
}

// D8: the password reaches psql through the environment and is re-quoted there,
// so it is never a shell word or an argv another process can read.
func TestPostgresEngine_SetPasswordKeepsTheSecretOutOfArgv(t *testing.T) {
	runner := &recordingRunner{}
	engine := newTestEngine(t, runner.run)

	if err := engine.SetPassword(context.Background(), "postgres", "n3w-pw"); err != nil {
		t.Fatalf("SetPassword: %v", err)
	}
	if len(runner.calls) != 1 {
		t.Fatalf("ran %d commands, want 1", len(runner.calls))
	}
	call := runner.calls[0]
	for _, arg := range call.Args {
		if strings.Contains(arg, "n3w-pw") {
			t.Fatalf("argv %v carries the password", call.Args)
		}
	}
	if strings.Contains(call.Stdin, "n3w-pw") {
		t.Errorf("SQL %q carries the password literally, want it read from the environment", call.Stdin)
	}
	if !slices.Contains(call.Env, "RDS_MASTER_PASSWORD=n3w-pw") {
		t.Errorf("env %v does not carry the password", call.Env)
	}
	if !strings.Contains(call.Stdin, "ALTER ROLE") {
		t.Errorf("SQL %q does not alter the role", call.Stdin)
	}
}

// psql echoes the failing statement, which here would carry the new password
// back into a reply the control plane records.
func TestPostgresEngine_SetPasswordRedactsTheSecretFromAFailure(t *testing.T) {
	runner := &recordingRunner{err: errors.New(`ERROR: syntax error at or near "n3w-pw"`)}
	engine := newTestEngine(t, runner.run)

	err := engine.SetPassword(context.Background(), "postgres", "n3w-pw")
	if err == nil {
		t.Fatal("SetPassword succeeded against a failing psql")
	}
	if strings.Contains(err.Error(), "n3w-pw") {
		t.Errorf("error %q leaks the password", err)
	}
	if !strings.Contains(err.Error(), "[REDACTED]") {
		t.Errorf("error %q does not mark the redaction", err)
	}
}

func TestPostgresEngine_SetPasswordRejectsAnIncompleteCommand(t *testing.T) {
	runner := &recordingRunner{}
	engine := newTestEngine(t, runner.run)

	if err := engine.SetPassword(context.Background(), "postgres", ""); err == nil {
		t.Error("SetPassword accepted an empty password")
	}
	if len(runner.calls) != 0 {
		t.Errorf("ran %d commands for an incomplete request, want 0", len(runner.calls))
	}
}

// The parameters land where rds-init installs them, so the next boot overwrites
// the file rather than shadowing it with a second copy.
func TestPostgresEngine_ApplyParametersInstallsAndReloads(t *testing.T) {
	runner := &recordingRunner{out: "shared_buffers\nmax_connections\n"}
	engine := newTestEngine(t, runner.run)

	pending, err := engine.ApplyParameters(context.Background(),
		[]handlers_rds.Parameter{{Name: "shared_buffers", Value: "256MB"}})
	if err != nil {
		t.Fatalf("ApplyParameters: %v", err)
	}

	installed, err := os.ReadFile(filepath.Join(engine.pgData, "conf.d", "10-rds-parameters.conf"))
	if err != nil {
		t.Fatalf("read the installed parameters: %v", err)
	}
	if !strings.Contains(string(installed), "shared_buffers = '256MB'") {
		t.Errorf("installed parameters = %q, want the resolved value", installed)
	}
	if len(runner.calls) != 3 {
		t.Fatalf("ran %d commands, want a config check, a reload and a pending-restart read", len(runner.calls))
	}
	// The check runs before the reload, so a value the engine would refuse never
	// reaches a running cluster.
	if !slices.Contains(runner.calls[0].Args, "-C") {
		t.Errorf("first command = %v, want the offline config check", runner.calls[0].Args)
	}
	if !strings.Contains(runner.calls[1].Stdin, "pg_reload_conf") {
		t.Errorf("second statement = %q, want the reload", runner.calls[1].Stdin)
	}
	if !strings.Contains(runner.calls[2].Stdin, "pending_restart") {
		t.Errorf("third statement = %q, want the pending-restart read", runner.calls[2].Stdin)
	}
	if len(pending) != 2 || pending[0] != "shared_buffers" || pending[1] != "max_connections" {
		t.Errorf("pending = %v, want both settings the engine reported", pending)
	}
}

// A reload that failed leaves the engine on its old values, so reporting the
// apply as successful would tell the customer a change took effect that did not.
func TestPostgresEngine_ApplyParametersSurfacesAFailedReload(t *testing.T) {
	runner := &recordingRunner{err: errors.New("could not connect to server")}
	engine := newTestEngine(t, runner.run)

	if _, err := engine.ApplyParameters(context.Background(),
		[]handlers_rds.Parameter{{Name: "work_mem", Value: "8MB"}}); err == nil {
		t.Fatal("ApplyParameters succeeded against a failing reload")
	}
}

// Through the service manager, so the supervisor records the engine as stopped
// and does not restart it underneath a VM that is going down.
func TestPostgresEngine_StopGoesThroughTheServiceManager(t *testing.T) {
	runner := &recordingRunner{}
	engine := newTestEngine(t, runner.run)

	if err := engine.Stop(context.Background()); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if len(runner.calls) != 1 {
		t.Fatalf("ran %d commands, want 1", len(runner.calls))
	}
	call := runner.calls[0]
	if filepath.Base(call.Name) != "rc-service" {
		t.Errorf("ran %q, want rc-service", call.Name)
	}
	if len(call.Args) != 2 || call.Args[0] != "postgresql" || call.Args[1] != "stop" {
		t.Errorf("args = %v, want [postgresql stop]", call.Args)
	}
}
