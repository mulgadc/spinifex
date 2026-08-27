package main

import (
	"context"
	"errors"
	"fmt"
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
			{Name: handlers_rds.CommandParamMasterUsername, Value: "mulgamaster"},
			{Name: handlers_rds.CommandParamMasterUserPassword, Value: "n3w-pw"},
		},
	})

	if reply.Status != handlers_rds.CommandStatusSucceeded {
		t.Fatalf("reply = %+v, want succeeded", reply)
	}
	if engine.username != "mulgamaster" || engine.password != "n3w-pw" {
		t.Errorf("engine got %q/%q, want mulgamaster/n3w-pw", engine.username, engine.password)
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

func testPostgresEngineMeta(t *testing.T) handlers_rds.Engine {
	t.Helper()
	meta, err := handlers_rds.LookupEngine(enginePostgres)
	if err != nil {
		t.Fatalf("LookupEngine(%s): %v", enginePostgres, err)
	}
	return meta
}

func newTestEngine(t *testing.T, run commandRunner) *postgresEngine {
	t.Helper()
	cfg := testEngineConfig(t)
	return newPostgresEngine(cfg, testPostgresEngineMeta(t), withPostgresReadBacks(cfg.EngineDataDir, run), nil, newPostgresProbe(cfg, staticProbe(0)))
}

// The same engine with the read-backs left to the runner, for the cases that are
// about what the engine does with each answer rather than about the rest of an
// apply.
func newScriptedTestEngine(t *testing.T, run commandRunner) *postgresEngine {
	t.Helper()
	cfg := testEngineConfig(t)
	return newPostgresEngine(cfg, testPostgresEngineMeta(t), run, nil, newPostgresProbe(cfg, staticProbe(0)))
}

func testEngineConfig(t *testing.T) config {
	t.Helper()
	cfg := testLoadConfig(t, enginePostgres)
	cfg.EngineDataDir = t.TempDir()
	if err := os.MkdirAll(filepath.Join(cfg.EngineDataDir, "conf.d"), 0o700); err != nil {
		t.Fatalf("create conf.d: %v", err)
	}
	// The guest's postgres user does not exist here, and the chown of the
	// installed parameter file has to resolve to something real.
	current, err := user.Current()
	if err != nil {
		t.Fatalf("resolve the current user: %v", err)
	}
	cfg.EngineUser = current.Username
	return cfg
}

// Answers the read-backs every apply runs against a live postmaster, from the
// datadir the case is working in: an engine serving TLS, a reload the postmaster
// took, and a pg_hba carrying whatever the agent last wrote. The call still
// reaches the runner, so a case counting commands sees them; one that is about
// the read-backs themselves builds its own engine.
func withPostgresReadBacks(dataDir string, run commandRunner) commandRunner {
	rule := filepath.Join(dataDir, postgresHBADir, postgresForceSSLRuleFile)
	return func(ctx context.Context, c command) (string, error) {
		out, err := run(ctx, c)
		if err != nil {
			return out, err
		}
		switch {
		case strings.Contains(c.Stdin, "SHOW ssl"):
			return "on\n", nil
		case strings.Contains(c.Stdin, "pg_reload_conf"):
			return "t\n", nil
		case strings.Contains(c.Stdin, "pg_hba_file_rules"):
			rules := 0
			if _, statErr := os.Stat(rule); statErr == nil {
				rules = postgresForceSSLRuleCount
			}
			return fmt.Sprintf("%d|0\n", rules), nil
		}
		return out, err
	}
}

// The password reaches psql through the environment and is re-quoted there,
// so it is never a shell word or an argv another process can read.
func TestPostgresEngine_SetPasswordKeepsTheSecretOutOfArgv(t *testing.T) {
	runner := &recordingRunner{}
	engine := newTestEngine(t, runner.run)

	if err := engine.SetPassword(context.Background(), "mulgamaster", "n3w-pw"); err != nil {
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

// psql interpolates the password into the ALTER ROLE before the server sees it,
// so a parameter group that turns statement logging on would write the rotated
// password into a postmaster.log every snapshot then carries.
func TestPostgresEngine_SetPasswordSilencesStatementLogging(t *testing.T) {
	runner := &recordingRunner{}
	engine := newTestEngine(t, runner.run)

	if err := engine.SetPassword(context.Background(), "mulgamaster", "n3w-pw"); err != nil {
		t.Fatalf("SetPassword: %v", err)
	}
	if len(runner.calls) != 1 {
		t.Fatalf("ran %d commands, want 1", len(runner.calls))
	}
	sql := runner.calls[0].Stdin
	alter := strings.Index(sql, "ALTER ROLE")
	if alter < 0 {
		t.Fatalf("SQL %q does not alter the role", sql)
	}
	for _, guard := range []string{
		"SET log_statement = 'none';",
		"SET log_min_duration_statement = -1;",
		"SET log_min_error_statement = 'panic';",
	} {
		at := strings.Index(sql, guard)
		if at < 0 {
			t.Errorf("SQL %q is missing %q", sql, guard)
			continue
		}
		if at > alter {
			t.Errorf("SQL %q sets %q only after the ALTER ROLE has already been logged", sql, guard)
		}
	}
}

// psql echoes the failing statement, which here would carry the new password
// back into a reply the control plane records.
func TestPostgresEngine_SetPasswordRedactsTheSecretFromAFailure(t *testing.T) {
	runner := &recordingRunner{err: errors.New(`ERROR: syntax error at or near "n3w-pw"`)}
	engine := newTestEngine(t, runner.run)

	err := engine.SetPassword(context.Background(), "mulgamaster", "n3w-pw")
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

	if err := engine.SetPassword(context.Background(), "mulgamaster", ""); err == nil {
		t.Error("SetPassword accepted an empty password")
	}
	if len(runner.calls) != 0 {
		t.Errorf("ran %d commands for an incomplete request, want 0", len(runner.calls))
	}
}

// The apply runs as the cluster superuser under peer auth, so a reserved
// name arriving in a command payload would rotate the bootstrap superuser's
// password rather than the customer's own role.
func TestPostgresEngine_SetPasswordRefusesReservedRoles(t *testing.T) {
	for _, username := range []string{"postgres", "rds_superuser", "rdsadmin", "pg_toast_owner", "PostGres"} {
		t.Run(username, func(t *testing.T) {
			runner := &recordingRunner{}
			engine := newTestEngine(t, runner.run)

			err := engine.SetPassword(context.Background(), username, "n3w-pw")
			if err == nil {
				t.Fatalf("SetPassword accepted the reserved role %q", username)
			}
			if len(runner.calls) != 0 {
				t.Errorf("ran %d commands for a reserved role, want 0", len(runner.calls))
			}
		})
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
	if len(runner.calls) != 5 {
		t.Fatalf("ran %d commands, want a config check, the TLS guard, a reload, the rule read-back and a pending-restart read", len(runner.calls))
	}
	// The check and the TLS guard both run before the reload, so neither a value
	// the engine would refuse nor a rule rejecting every client reaches a running
	// cluster. The read-back of the rules comes after, since the reload is what
	// puts them in force.
	if !slices.Contains(runner.calls[0].Args, "-C") {
		t.Errorf("first command = %v, want the offline config check", runner.calls[0].Args)
	}
	if !strings.Contains(runner.calls[1].Stdin, "SHOW ssl") {
		t.Errorf("second statement = %q, want the serving-TLS guard", runner.calls[1].Stdin)
	}
	if !strings.Contains(runner.calls[2].Stdin, "pg_reload_conf") {
		t.Errorf("third statement = %q, want the reload", runner.calls[2].Stdin)
	}
	if !strings.Contains(runner.calls[3].Stdin, "pg_hba_file_rules") {
		t.Errorf("fourth statement = %q, want the client authentication read-back", runner.calls[3].Stdin)
	}
	if !strings.Contains(runner.calls[4].Stdin, "pending_restart") {
		t.Errorf("fifth statement = %q, want the pending-restart read", runner.calls[4].Stdin)
	}
	if len(pending) != 2 || pending[0] != "shared_buffers" || pending[1] != "max_connections" {
		t.Errorf("pending = %v, want both settings the engine reported", pending)
	}
}

// A reload that failed leaves the engine on its old values, so reporting the
// apply as successful would tell the customer a change took effect that did not.
func TestPostgresEngine_ApplyParametersSurfacesAFailedReload(t *testing.T) {
	runner := &reloadFailingRunner{reloadErr: errors.New("reload was rejected")}
	engine := newTestEngine(t, runner.run)

	if _, err := engine.ApplyParameters(context.Background(),
		[]handlers_rds.Parameter{{Name: "work_mem", Value: "8MB"}}); err == nil {
		t.Fatal("ApplyParameters succeeded against a failing reload")
	}
}

// pg_reload_conf() returns f when it could not signal the postmaster, and psql
// exits 0 all the same. Every read-back after it parses files from disk, so an
// apply that trusted the exit status would verify clean against a server still
// serving the old rules.
func TestPostgresEngine_ApplyParametersSurfacesAnUnsignalledReload(t *testing.T) {
	runner := &tlsReadBackRunner{ssl: "on", ruleRows: postgresForceSSLRuleCount, reload: "f"}
	engine := newScriptedTestEngine(t, runner.run)

	_, err := engine.ApplyParameters(context.Background(),
		[]handlers_rds.Parameter{{Name: "work_mem", Value: "8MB"}})
	if err == nil || !strings.Contains(err.Error(), `pg_reload_conf() returned "f"`) {
		t.Fatalf("ApplyParameters error = %v, want a refusal naming the answer the postmaster gave", err)
	}
}

type reloadFailingRunner struct {
	recordingRunner

	reloadErr error
}

func (r *reloadFailingRunner) run(ctx context.Context, c command) (string, error) {
	if strings.Contains(c.Stdin, "pg_reload_conf") {
		r.calls = append(r.calls, c)
		return "", r.reloadErr
	}
	return r.recordingRunner.run(ctx, c)
}

// A failed instance cannot reload through psql. Its corrected, parser-checked
// include must start the service and remain installed as the serving set.
func TestPostgresEngine_ApplyParametersRestartsOnARepairSetWhileEngineIsDown(t *testing.T) {
	runner := &recordingRunner{}
	engine := newTestEngine(t, runner.run)
	codes := []int{2, 2, 0}
	probeCall := 0
	cfg := testProbeConfig()
	engine.probe = newPostgresProbe(cfg, func(context.Context, string, ...string) (int, string, error) {
		code := codes[min(probeCall, len(codes)-1)]
		probeCall++
		return code, "", nil
	})
	engine.repairTimeout = 100 * time.Millisecond
	engine.repairPoll = time.Millisecond
	params := []handlers_rds.Parameter{
		{Name: "shared_buffers", Value: "32768"},
		{Name: "work_mem", Value: "8192"},
	}

	pending, err := engine.ApplyParameters(t.Context(), params)
	if err != nil {
		t.Fatalf("ApplyParameters: %v", err)
	}
	if len(pending) != 0 {
		t.Errorf("pending = %v, want the restarted engine's actual pending set", pending)
	}
	if len(runner.calls) != 3 {
		t.Fatalf("calls = %+v, want config check, service restart and pending read", runner.calls)
	}
	if !slices.Contains(runner.calls[0].Args, "-C") {
		t.Errorf("first call = %+v, want the offline config check", runner.calls[0])
	}
	if !slices.Equal(runner.calls[1].Args, []string{"restart", "spinifex-postgresql.service"}) {
		t.Errorf("second call args = %v, want the service restart", runner.calls[1].Args)
	}
	if !strings.Contains(runner.calls[2].Stdin, "pending_restart") {
		t.Errorf("third call = %+v, want the pending-restart read", runner.calls[2])
	}
	installed, err := os.ReadFile(engine.params.installedPath())
	if err != nil {
		t.Fatalf("read installed parameters: %v", err)
	}
	if !strings.Contains(string(installed), "work_mem = '8192'") {
		t.Errorf("installed parameters = %q, want the repair set retained", installed)
	}
}

// Answers the two read-backs an apply runs from fields the case sets, so what
// the engine does with each answer is what is under test.
type tlsReadBackRunner struct {
	recordingRunner

	ssl        string
	ruleRows   int
	brokenRows int
	// What pg_reload_conf() answered. Empty is the postmaster having taken the
	// signal, which is every case but the one about it not having.
	reload string
}

func (r *tlsReadBackRunner) run(ctx context.Context, c command) (string, error) {
	switch {
	case strings.Contains(c.Stdin, "SHOW ssl"):
		r.calls = append(r.calls, c)
		return r.ssl + "\n", nil
	case strings.Contains(c.Stdin, "pg_reload_conf"):
		r.calls = append(r.calls, c)
		if r.reload == "" {
			return "t\n", nil
		}
		return r.reload + "\n", nil
	case strings.Contains(c.Stdin, "pg_hba_file_rules"):
		r.calls = append(r.calls, c)
		return fmt.Sprintf("%d|%d\n", r.ruleRows, r.brokenRows), nil
	}
	return r.recordingRunner.run(ctx, c)
}

func forceSSLParameter(t *testing.T, value string) []handlers_rds.Parameter {
	t.Helper()
	return []handlers_rds.Parameter{
		{Name: "work_mem", Value: "4096"},
		{Name: testPostgresEngineMeta(t).TLSEnforcementParameter(), Value: value},
	}
}

// PostgreSQL has no server setting for this, so the value in the parameter file
// is inert: the rule the generated pg_hba includes is the whole of it.
func TestPostgresEngine_ApplyParametersWritesTheEnforcementRule(t *testing.T) {
	runner := &tlsReadBackRunner{ssl: "on", ruleRows: postgresForceSSLRuleCount}
	engine := newScriptedTestEngine(t, runner.run)

	if _, err := engine.ApplyParameters(t.Context(), forceSSLParameter(t, "1")); err != nil {
		t.Fatalf("ApplyParameters: %v", err)
	}
	rule, err := os.ReadFile(engine.forceSSLRulePath())
	if err != nil {
		t.Fatalf("read the enforcement rule: %v", err)
	}
	if string(rule) != postgresForceSSLRules {
		t.Errorf("enforcement rule = %q, want the constant rds-init also writes", rule)
	}
}

// A snapshot carries hba.d with it, so restoring one taken while enforcing into
// a group that turns enforcement off has to take the rule with it.
func TestPostgresEngine_ApplyParametersRemovesAStaleEnforcementRule(t *testing.T) {
	runner := &tlsReadBackRunner{ssl: "on"}
	engine := newScriptedTestEngine(t, runner.run)
	if err := os.MkdirAll(filepath.Dir(engine.forceSSLRulePath()), 0o700); err != nil {
		t.Fatalf("create hba.d: %v", err)
	}
	if err := os.WriteFile(engine.forceSSLRulePath(), []byte(postgresForceSSLRules), 0o600); err != nil {
		t.Fatalf("write the rule the restored volume carried: %v", err)
	}

	if _, err := engine.ApplyParameters(t.Context(), forceSSLParameter(t, "0")); err != nil {
		t.Fatalf("ApplyParameters: %v", err)
	}
	if _, err := os.Stat(engine.forceSSLRulePath()); !os.IsNotExist(err) {
		t.Errorf("the stale enforcement rule is still in place (stat err = %v)", err)
	}
}

// The ordinary state of an instance whose resolved set predates the parameter,
// and the whole of the migration: it begins requiring TLS with no control-plane
// work at all.
func TestPostgresEngine_ApplyParametersEnforcesOnAnAbsentKey(t *testing.T) {
	runner := &tlsReadBackRunner{ssl: "on", ruleRows: postgresForceSSLRuleCount}
	engine := newScriptedTestEngine(t, runner.run)

	if _, err := engine.ApplyParameters(t.Context(),
		[]handlers_rds.Parameter{{Name: "work_mem", Value: "4096"}}); err != nil {
		t.Fatalf("ApplyParameters: %v", err)
	}
	if _, err := os.Stat(engine.forceSSLRulePath()); err != nil {
		t.Errorf("a set predating the parameter did not enforce (stat err = %v)", err)
	}
}

// The resolver canonicalises every boolean, so a value that is neither means the
// file was written by something other than the platform. Reading an unparsable
// security setting as off is the one choice not open here.
func TestPostgresEngine_ApplyParametersRefusesAnUnparsableEnforcementValue(t *testing.T) {
	runner := &tlsReadBackRunner{ssl: "on"}
	engine := newScriptedTestEngine(t, runner.run)

	_, err := engine.ApplyParameters(t.Context(), forceSSLParameter(t, "yes"))
	if err == nil || !strings.Contains(err.Error(), "neither 1 nor 0") {
		t.Fatalf("ApplyParameters error = %v, want a refusal naming the unreadable value", err)
	}
	if _, statErr := os.Stat(engine.params.installedPath()); !os.IsNotExist(statErr) {
		t.Errorf("the refused set is still installed (stat err = %v)", statErr)
	}
}

// Without this, a live enable against an engine started with no certificate
// would reject every subsequent connection and report the parameter applied.
func TestPostgresEngine_ApplyParametersRefusesToEnforceWithoutTLS(t *testing.T) {
	runner := &tlsReadBackRunner{ssl: "off"}
	engine := newScriptedTestEngine(t, runner.run)

	_, err := engine.ApplyParameters(t.Context(), forceSSLParameter(t, "1"))
	if err == nil || !strings.Contains(err.Error(), "serving without TLS") {
		t.Fatalf("ApplyParameters error = %v, want a refusal naming the engine's own TLS state", err)
	}
	if _, statErr := os.Stat(engine.forceSSLRulePath()); !os.IsNotExist(statErr) {
		t.Errorf("the enforcement rule was written anyway (stat err = %v)", statErr)
	}
}

// PostgreSQL discards a pg_hba it cannot parse and keeps the rules it is already
// serving, which presents as an apply that succeeded and enforces nothing. The
// shape is asserted as well as the errors, because our own derivation writing
// the wrong thing is the failure that will actually happen.
func TestPostgresEngine_ApplyParametersFailsAnUnverifiableEnforcement(t *testing.T) {
	tests := []struct {
		name       string
		value      string
		ruleRows   int
		brokenRows int
		want       string
	}{
		{name: "rule missing", value: "1", ruleRows: 0, want: "carries 0 TLS enforcement rules, want 2"},
		{name: "rule present when it should not be", value: "0", ruleRows: 2, want: "carries 2 TLS enforcement rules, want 0"},
		{name: "a rule the engine could not parse", value: "1", ruleRows: 2, brokenRows: 1, want: "could not parse"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			runner := &tlsReadBackRunner{ssl: "on", ruleRows: tc.ruleRows, brokenRows: tc.brokenRows}
			engine := newScriptedTestEngine(t, runner.run)

			_, err := engine.ApplyParameters(t.Context(), forceSSLParameter(t, tc.value))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("ApplyParameters error = %v, want %q", err, tc.want)
			}
		})
	}
}

// A rejected set has to take its enforcement with it. Withdrawing one without
// the other would leave the instance serving the previous parameters under the
// rejected set's TLS posture.
func TestPostgresEngine_ApplyParametersWithdrawsBothFilesTogether(t *testing.T) {
	runner := &tlsReadBackRunner{ssl: "on", ruleRows: postgresForceSSLRuleCount}
	engine := newScriptedTestEngine(t, runner.run)
	if _, err := engine.ApplyParameters(t.Context(), forceSSLParameter(t, "1")); err != nil {
		t.Fatalf("first ApplyParameters: %v", err)
	}

	// Turning enforcement off, verified against a pg_hba that still carries the
	// rule — so the apply fails after both files have already been replaced.
	if _, err := engine.ApplyParameters(t.Context(), forceSSLParameter(t, "0")); err == nil {
		t.Fatal("ApplyParameters succeeded against an unverifiable enforcement")
	}
	installed, err := os.ReadFile(engine.params.installedPath())
	if err != nil {
		t.Fatalf("read the installed parameters: %v", err)
	}
	if !strings.Contains(string(installed), "rds.force_ssl = '1'") {
		t.Errorf("installed parameters = %q, want the previous accepted set restored", installed)
	}
	if _, err := os.Stat(engine.forceSSLRulePath()); err != nil {
		t.Errorf("the enforcement of the previous set was not restored with it (stat err = %v)", err)
	}
	// The reload already adopted the rejected files, so putting them back is not
	// enough on its own.
	if !strings.Contains(runner.calls[len(runner.calls)-1].Stdin, "pg_reload_conf") {
		t.Errorf("last statement = %q, want a reload taking the engine off the withdrawn set", runner.calls[len(runner.calls)-1].Stdin)
	}
}

// A service restart does not re-run rds-init, so a restore stopping at the
// parameter file would bring the last known good set up under the enforcement of
// the set that was just rejected.
func TestPostgresEngine_RestoreLastKnownGoodReDerivesEnforcement(t *testing.T) {
	runner := &tlsReadBackRunner{ssl: "on", ruleRows: postgresForceSSLRuleCount}
	engine := newScriptedTestEngine(t, runner.run)

	if _, err := engine.ApplyParameters(t.Context(), forceSSLParameter(t, "1")); err != nil {
		t.Fatalf("ApplyParameters(enforcing): %v", err)
	}
	if err := engine.RecordServingParameters(t.Context()); err != nil {
		t.Fatalf("RecordServingParameters: %v", err)
	}
	runner.ruleRows = 0
	if _, err := engine.ApplyParameters(t.Context(), forceSSLParameter(t, "0")); err != nil {
		t.Fatalf("ApplyParameters(not enforcing): %v", err)
	}
	if _, err := os.Stat(engine.forceSSLRulePath()); !os.IsNotExist(err) {
		t.Fatalf("the enforcement rule survived the set that turned it off (stat err = %v)", err)
	}

	engine.probe = stubProbe(t, 2, nil)
	restored, err := engine.RestoreLastKnownGoodParameters(t.Context())
	if err != nil {
		t.Fatalf("RestoreLastKnownGoodParameters: %v", err)
	}
	if !restored {
		t.Fatal("did not restore a set that differs from the last known good")
	}
	if _, err := os.Stat(engine.forceSSLRulePath()); err != nil {
		t.Errorf("the restored set's enforcement was not re-derived (stat err = %v)", err)
	}
}

func TestPostgresEngine_ApplyParametersKeepsRepairSetWhenRestartTimesOut(t *testing.T) {
	runner := &recordingRunner{}
	engine := newTestEngine(t, runner.run)
	engine.probe = stubProbe(t, 2, nil)
	engine.repairTimeout = 10 * time.Millisecond
	engine.repairPoll = time.Millisecond

	_, err := engine.ApplyParameters(t.Context(), []handlers_rds.Parameter{{Name: "work_mem", Value: "8192"}})
	if err == nil || !strings.Contains(err.Error(), "wait for the engine") {
		t.Fatalf("ApplyParameters error = %v, want the repair wait failure", err)
	}
	installed, readErr := os.ReadFile(engine.params.installedPath())
	if readErr != nil {
		t.Fatalf("read installed parameters: %v", readErr)
	}
	if !strings.Contains(string(installed), "work_mem = '8192'") {
		t.Errorf("installed parameters = %q, want the checked repair set retained", installed)
	}
}
