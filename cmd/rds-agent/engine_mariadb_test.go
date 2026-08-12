package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	handlers_rds "github.com/mulgadc/spinifex/spinifex/handlers/rds"
)

// Stands in for the control plane's own definition of the engine. It mirrors the
// production wiring exactly, including that a name the catalog does not carry
// counts as static: a rule the fake inverted would hide the failure it exists to
// prevent.
func testMariaDBRules() controlPlaneRules {
	applyTypes := map[string]string{
		"innodb_buffer_pool_size": handlers_rds.ApplyTypeStatic,
		"innodb_log_file_size":    handlers_rds.ApplyTypeStatic,
		"max_connections":         handlers_rds.ApplyTypeDynamic,
		"long_query_time":         handlers_rds.ApplyTypeDynamic,
		"time_zone":               handlers_rds.ApplyTypeDynamic,
	}
	reserved := []string{"root", "mysql", "mariadb.sys", "rdsadmin", "public"}

	return controlPlaneRules{
		validateUsername: func(username string) error {
			lower := strings.ToLower(username)
			if slices.Contains(reserved, lower) || strings.HasPrefix(lower, "mysql.") || strings.HasPrefix(lower, "mariadb.") {
				return fmt.Errorf("MasterUsername %q is reserved by mariadb", username)
			}
			for _, r := range username {
				if !isTestIdentifierRune(r) {
					return fmt.Errorf("MasterUsername may contain only letters, digits and underscores")
				}
			}
			return nil
		},
		isStatic: func(name string) bool {
			applyType, ok := applyTypes[name]
			return !ok || applyType == handlers_rds.ApplyTypeStatic
		},
		catalogName: func(optionFileName string) string {
			if optionFileName == "default_time_zone" {
				return "time_zone"
			}
			return optionFileName
		},
	}
}

func isTestIdentifierRune(r rune) bool {
	return (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '_'
}

func newTestMariaDBEngine(t *testing.T, run commandRunner) *mariadbEngine {
	t.Helper()
	cfg := testLoadConfig(t, engineMariaDB)
	cfg.DataMount = t.TempDir()
	if err := os.MkdirAll(filepath.Join(cfg.DataMount, "conf.d"), 0o750); err != nil {
		t.Fatalf("create conf.d: %v", err)
	}
	// The guest's mysql user does not exist here, and the chown of the installed
	// parameter file has to resolve to something real.
	current, err := user.Current()
	if err != nil {
		t.Fatalf("resolve the current user: %v", err)
	}
	cfg.EngineUser = current.Username

	state := engineServing
	return newMariaDBEngine(cfg, testMariaDBRules(), run, nil, drivenProbe(&state))
}

// The wiring the guest takes its classification from, checked against a catalog
// that is registered: what a spec says about a parameter is what the guest must
// see, and an unlisted name must read as static rather than as dynamic.
func TestControlPlaneRulesFrom_TakesTheClassificationFromTheCatalog(t *testing.T) {
	rules := controlPlaneRulesFrom(postgresEngineMeta)

	for name, want := range map[string]bool{"shared_buffers": true, "work_mem": false} {
		spec, ok := postgresEngineMeta.LookupParameter(name)
		if !ok {
			t.Fatalf("the catalog no longer carries %s", name)
		}
		if (spec.ApplyType == handlers_rds.ApplyTypeStatic) != want {
			t.Fatalf("%s is %s in the catalog, so this test is asserting the wrong way round", name, spec.ApplyType)
		}
		if got := rules.isStatic(name); got != want {
			t.Errorf("isStatic(%s) = %v, want %v", name, got, want)
		}
	}
	if !rules.isStatic("not_a_parameter_any_catalog_carries") {
		t.Error("a name the catalog does not carry read as dynamic, so it would be issued as a live SET GLOBAL")
	}
	if rules.validateUsername("postgres") == nil {
		t.Error("the username rule does not refuse a reserved role")
	}
}

// The client has no parameter quoting of its own, so the password is built into
// the statement here. It must reach the process on stdin and nowhere else.
func TestMariaDBEngine_SetPasswordKeepsTheSecretOutOfArgvAndEnv(t *testing.T) {
	runner := &recordingRunner{}
	engine := newTestMariaDBEngine(t, runner.run)

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
	for _, env := range call.Env {
		if strings.Contains(env, "n3w-pw") {
			t.Fatalf("env %v carries the password, which the client would not read anyway", call.Env)
		}
	}
	if !strings.Contains(call.Stdin, "ALTER USER 'mulgamaster'@'%' IDENTIFIED BY 'n3w-pw';") {
		t.Errorf("SQL %q does not rotate the master on the wildcard host", call.Stdin)
	}
}

// ValidateMasterUserPassword permits both of these, and the client offers no
// equivalent of psql's parameter quoting, so the escaping here is what keeps a
// password from ending the literal it is inside.
func TestMariaDBEngine_SetPasswordEscapesQuotesAndBackslashes(t *testing.T) {
	runner := &recordingRunner{}
	engine := newTestMariaDBEngine(t, runner.run)

	if err := engine.SetPassword(context.Background(), "mulgamaster", `a'b\c`); err != nil {
		t.Fatalf("SetPassword: %v", err)
	}
	sql := runner.calls[0].Stdin
	if !strings.Contains(sql, `IDENTIFIED BY 'a''b\\c';`) {
		t.Errorf("SQL %q does not escape both the quote and the backslash", sql)
	}
	// The escaping assumes a mode where backslash escapes are on, so the statement
	// cannot inherit whatever sql_mode the customer's parameter group set.
	if !strings.Contains(sql, "SET SESSION sql_mode = 'NO_ENGINE_SUBSTITUTION';") {
		t.Errorf("SQL %q does not pin the mode its escaping assumes", sql)
	}
}

// The password is interpolated into the statement, so a parameter group that
// turned the general or slow log on would copy it into a log file every snapshot
// then carries. Both are silenced for this session only: a rotation must not
// leave the customer's own logging switched off behind it.
func TestMariaDBEngine_SetPasswordSilencesStatementLoggingForItsSessionOnly(t *testing.T) {
	runner := &recordingRunner{}
	engine := newTestMariaDBEngine(t, runner.run)

	if err := engine.SetPassword(context.Background(), "mulgamaster", "n3w-pw"); err != nil {
		t.Fatalf("SetPassword: %v", err)
	}
	sql := runner.calls[0].Stdin
	alter := strings.Index(sql, "ALTER USER")
	if alter < 0 {
		t.Fatalf("SQL %q does not alter the user", sql)
	}
	for _, guard := range []string{"SET SESSION sql_log_off = 1;", "SET SESSION slow_query_log = 0;"} {
		at := strings.Index(sql, guard)
		if at < 0 {
			t.Errorf("SQL %q is missing %q", sql, guard)
			continue
		}
		if at > alter {
			t.Errorf("SQL %q sets %q only after the statement has already been logged", sql, guard)
		}
	}
	if strings.Contains(sql, "SET GLOBAL general_log") || strings.Contains(sql, "SET GLOBAL slow_query_log") {
		t.Errorf("SQL %q turns logging off globally, which outlives the rotation", sql)
	}
}

// The client echoes the statement it failed on, which carries the escaped form
// of the password rather than the raw one — so redacting only the raw one would
// leak exactly the passwords that needed escaping.
func TestMariaDBEngine_SetPasswordRedactsBothFormsOfTheSecret(t *testing.T) {
	secret := `a'b\c`
	runner := &recordingRunner{err: errors.New(`ERROR 1064 near "IDENTIFIED BY 'a''b\\c'"`)}
	engine := newTestMariaDBEngine(t, runner.run)

	err := engine.SetPassword(context.Background(), "mulgamaster", secret)
	if err == nil {
		t.Fatal("SetPassword succeeded against a failing client")
	}
	if strings.Contains(err.Error(), secret) || strings.Contains(err.Error(), sqlLiteral(secret)) {
		t.Errorf("error %q leaks the password", err)
	}
	if !strings.Contains(err.Error(), "[REDACTED]") {
		t.Errorf("error %q does not mark the redaction", err)
	}
}

// The apply runs as the server's own superuser over the socket, and the name is
// interpolated into the statement rather than quoted by the client, so both
// halves of the rule are refused before any SQL is built.
func TestMariaDBEngine_SetPasswordRefusesNamesTheEngineWillNotTake(t *testing.T) {
	for _, username := range []string{"root", "mysql", "mariadb.sys", "rdsadmin", "PUBLIC", "mysql.infoschema", `ma'ster`} {
		t.Run(username, func(t *testing.T) {
			runner := &recordingRunner{}
			engine := newTestMariaDBEngine(t, runner.run)

			if err := engine.SetPassword(context.Background(), username, "n3w-pw"); err == nil {
				t.Fatalf("SetPassword accepted %q", username)
			}
			if len(runner.calls) != 0 {
				t.Errorf("ran %d commands for a refused name, want 0", len(runner.calls))
			}
		})
	}
}

func TestMariaDBEngine_SetPasswordRejectsAnIncompleteCommand(t *testing.T) {
	runner := &recordingRunner{}
	engine := newTestMariaDBEngine(t, runner.run)

	if err := engine.SetPassword(context.Background(), "mulgamaster", ""); err == nil {
		t.Error("SetPassword accepted an empty password")
	}
	if len(runner.calls) != 0 {
		t.Errorf("ran %d commands for an incomplete request, want 0", len(runner.calls))
	}
}

// A running server re-reads no configuration file, so the dynamic half has to
// reach it as SET GLOBAL. The static half is installed and waits for a restart,
// which is what comes back as pending.
func TestMariaDBEngine_ApplyParametersSetsOnlyTheDynamicHalfLive(t *testing.T) {
	runner := &recordingRunner{}
	engine := newTestMariaDBEngine(t, runner.run)

	pending, err := engine.ApplyParameters(context.Background(), []handlers_rds.Parameter{
		{Name: "max_connections", Value: "200"},
		{Name: "innodb_buffer_pool_size", Value: "1073741824"},
	})
	if err != nil {
		t.Fatalf("ApplyParameters: %v", err)
	}

	installed, err := os.ReadFile(engine.params.installedPath())
	if err != nil {
		t.Fatalf("read the installed parameters: %v", err)
	}
	if !strings.HasPrefix(string(installed), "[mysqld]\n") {
		t.Errorf("installed parameters = %q, want the group header rds-init writes", installed)
	}
	for _, want := range []string{"max_connections = '200'", "innodb_buffer_pool_size = '1073741824'"} {
		if !strings.Contains(string(installed), want) {
			t.Errorf("installed parameters = %q, want %q", installed, want)
		}
	}

	if len(runner.calls) != 1 {
		t.Fatalf("ran %d commands, want the one live apply", len(runner.calls))
	}
	sql := runner.calls[0].Stdin
	if !strings.Contains(sql, "SET GLOBAL max_connections = '200';") {
		t.Errorf("SQL %q does not set the dynamic value live", sql)
	}
	if strings.Contains(sql, "innodb_buffer_pool_size") {
		t.Errorf("SQL %q sets a static value live, which the server would refuse", sql)
	}
	if !slices.Equal(pending, []string{"innodb_buffer_pool_size"}) {
		t.Errorf("pending = %v, want the static setting the engine has not adopted", pending)
	}
}

// A set with nothing dynamic in it must not open a connection at all: the
// installed file is the whole of the change.
func TestMariaDBEngine_ApplyParametersRunsNoStatementForAStaticOnlySet(t *testing.T) {
	runner := &recordingRunner{}
	engine := newTestMariaDBEngine(t, runner.run)

	if _, err := engine.ApplyParameters(context.Background(),
		[]handlers_rds.Parameter{{Name: "innodb_log_file_size", Value: "268435456"}}); err != nil {
		t.Fatalf("ApplyParameters: %v", err)
	}
	if len(runner.calls) != 0 {
		t.Errorf("ran %d commands for a static-only set, want 0", len(runner.calls))
	}
}

// A value the server refuses leaves the installed file back where it was, so it
// does not sit on the data volume turning the next restart into a boot loop.
func TestMariaDBEngine_ApplyParametersRollsBackAValueTheServerRefuses(t *testing.T) {
	runner := &recordingRunner{}
	engine := newTestMariaDBEngine(t, runner.run)

	if _, err := engine.ApplyParameters(context.Background(),
		[]handlers_rds.Parameter{{Name: "max_connections", Value: "200"}}); err != nil {
		t.Fatalf("first ApplyParameters: %v", err)
	}

	runner.err = errors.New("ERROR 1231 (42000): Variable 'max_connections' can't be set to the value of '0'")
	if _, err := engine.ApplyParameters(context.Background(),
		[]handlers_rds.Parameter{{Name: "max_connections", Value: "0"}}); err == nil {
		t.Fatal("ApplyParameters succeeded against a value the server refused")
	}

	installed, err := os.ReadFile(engine.params.installedPath())
	if err != nil {
		t.Fatalf("read the installed parameters: %v", err)
	}
	if !strings.Contains(string(installed), "max_connections = '200'") {
		t.Errorf("installed parameters = %q, want the previous accepted set restored", installed)
	}
}

// The whole of the pending-restart answer, since MariaDB has none of its own:
// the installed drop-in against the one the server actually started on, over the
// static keys only.
func TestMariaDBEngine_PendingRestartComparesTheInstalledSetAgainstTheServingCopy(t *testing.T) {
	engine := newTestMariaDBEngine(t, (&recordingRunner{}).run)
	write := func(path, body string) {
		t.Helper()
		if err := os.WriteFile(path, []byte("[mysqld]\n"+body), 0o600); err != nil {
			t.Fatalf("write %s: %v", path, err)
		}
	}
	pending := func() []string {
		t.Helper()
		names, err := engine.pendingRestartParameters(t.Context())
		if err != nil {
			t.Fatalf("pendingRestartParameters: %v", err)
		}
		return names
	}

	// A fresh boot: rds-init wrote the two together, so nothing is waiting.
	write(engine.params.installedPath(), "innodb_buffer_pool_size = '1073741824'\nmax_connections = '100'\n")
	write(engine.params.servingPath(), "innodb_buffer_pool_size = '1073741824'\nmax_connections = '100'\n")
	if names := pending(); len(names) != 0 {
		t.Errorf("pending = %v, want nothing on a set the engine started on", names)
	}

	// A dynamic value the server took live is not pending: it was adopted without
	// a restart, which is exactly what the serving copy not moving records.
	write(engine.params.installedPath(), "innodb_buffer_pool_size = '1073741824'\nmax_connections = '200'\n")
	if names := pending(); len(names) != 0 {
		t.Errorf("pending = %v, want nothing for a value adopted live", names)
	}

	// A static value is.
	write(engine.params.installedPath(), "innodb_buffer_pool_size = '2147483648'\nmax_connections = '200'\n")
	if names := pending(); !slices.Equal(names, []string{"innodb_buffer_pool_size"}) {
		t.Errorf("pending = %v, want the static setting", names)
	}

	// So is a static setting the group stopped naming, which reverts to its
	// default at the next start.
	write(engine.params.installedPath(), "max_connections = '200'\n")
	if names := pending(); !slices.Equal(names, []string{"innodb_buffer_pool_size"}) {
		t.Errorf("pending = %v, want the withdrawn static setting", names)
	}

	// MariaDB reads - and _ as the same character, so one setting spelled two ways
	// is not two settings.
	write(engine.params.installedPath(), "innodb-buffer-pool-size = '1073741824'\nmax_connections = '200'\n")
	if names := pending(); len(names) != 0 {
		t.Errorf("pending = %v, want the two spellings of one setting to compare equal", names)
	}
}

// rds-init writes the serving copy beside the installed one on every boot, so a
// set with no copy beside it is one the engine has not started on. Promoting it
// would point the rollback at a configuration that has never served.
func TestMariaDBEngine_PendingRestartTreatsAMissingServingCopyAsUnadopted(t *testing.T) {
	engine := newTestMariaDBEngine(t, (&recordingRunner{}).run)
	if err := os.WriteFile(engine.params.installedPath(),
		[]byte("[mysqld]\ninnodb_buffer_pool_size = '1073741824'\nmax_connections = '100'\n"), 0o600); err != nil {
		t.Fatalf("write installed parameters: %v", err)
	}

	names, err := engine.pendingRestartParameters(t.Context())
	if err != nil {
		t.Fatalf("pendingRestartParameters: %v", err)
	}
	if !slices.Equal(names, []string{"innodb_buffer_pool_size"}) {
		t.Errorf("pending = %v, want the static half of a set the engine never started on", names)
	}
}

// The restart the agent performs is the only thing that moves the serving copy,
// which is what makes a fresh start clear whatever was pending.
func TestMariaDBEngine_RestartRefreshesTheServingCopy(t *testing.T) {
	runner := &recordingRunner{}
	engine := newTestMariaDBEngine(t, runner.run)
	installed := []byte("[mysqld]\ninnodb_buffer_pool_size = '2147483648'\n")
	if err := os.WriteFile(engine.params.installedPath(), installed, 0o600); err != nil {
		t.Fatalf("write installed parameters: %v", err)
	}
	if err := os.WriteFile(engine.params.servingPath(), []byte("[mysqld]\ninnodb_buffer_pool_size = '1073741824'\n"), 0o600); err != nil {
		t.Fatalf("write serving parameters: %v", err)
	}

	if err := engine.Restart(context.Background()); err != nil {
		t.Fatalf("Restart: %v", err)
	}

	serving, err := os.ReadFile(engine.params.servingPath())
	if err != nil {
		t.Fatalf("read the serving copy: %v", err)
	}
	if string(serving) != string(installed) {
		t.Errorf("serving copy = %q, want the set the engine was just started on", serving)
	}
	names, err := engine.pendingRestartParameters(t.Context())
	if err != nil {
		t.Fatalf("pendingRestartParameters: %v", err)
	}
	if len(names) != 0 {
		t.Errorf("pending = %v, want a restart to have cleared it", names)
	}
	// The copy the comparison reads must never be parsed as a second set of
	// settings by the engine's own include glob.
	if strings.HasSuffix(engine.params.servingPath(), ".cnf") {
		t.Errorf("the serving copy at %s would be included by the engine", engine.params.servingPath())
	}
}

// The rollback target advances only once a restart has proved the installed
// static values can serve, not when a dynamic apply merely set them live.
func TestMariaDBEngine_LastKnownGoodWaitsForARestartToAdoptAStaticSet(t *testing.T) {
	runner := &recordingRunner{}
	engine := newTestMariaDBEngine(t, runner.run)

	if _, err := engine.ApplyParameters(t.Context(),
		[]handlers_rds.Parameter{{Name: "innodb_buffer_pool_size", Value: "1073741824"}}); err != nil {
		t.Fatalf("ApplyParameters: %v", err)
	}
	if err := engine.RecordServingParameters(t.Context()); err != nil {
		t.Fatalf("RecordServingParameters: %v", err)
	}
	if _, err := os.Stat(engine.params.lastGoodPath()); !os.IsNotExist(err) {
		t.Errorf("a set the engine has not restarted onto became the rollback target (stat err = %v)", err)
	}

	if err := engine.Restart(t.Context()); err != nil {
		t.Fatalf("Restart: %v", err)
	}
	if err := engine.RecordServingParameters(t.Context()); err != nil {
		t.Fatalf("RecordServingParameters after the restart: %v", err)
	}
	lastGood, err := os.ReadFile(engine.params.lastGoodPath())
	if err != nil {
		t.Fatalf("read the last known good parameters: %v", err)
	}
	if !strings.Contains(string(lastGood), "innodb_buffer_pool_size = '1073741824'") {
		t.Errorf("last known good = %q, want the set the engine restarted onto", lastGood)
	}
}

// A dynamic apply is adopted without a restart, so it may advance the rollback
// target on the next healthy beat rather than waiting for one.
func TestMariaDBEngine_LastKnownGoodAdvancesOnADynamicApply(t *testing.T) {
	runner := &recordingRunner{}
	engine := newTestMariaDBEngine(t, runner.run)
	if err := os.WriteFile(engine.params.servingPath(), []byte("[mysqld]\n"), 0o600); err != nil {
		t.Fatalf("write serving parameters: %v", err)
	}

	if _, err := engine.ApplyParameters(t.Context(),
		[]handlers_rds.Parameter{{Name: "max_connections", Value: "200"}}); err != nil {
		t.Fatalf("ApplyParameters: %v", err)
	}
	if err := engine.RecordServingParameters(t.Context()); err != nil {
		t.Fatalf("RecordServingParameters: %v", err)
	}

	lastGood, err := os.ReadFile(engine.params.lastGoodPath())
	if err != nil {
		t.Fatalf("read the last known good parameters: %v", err)
	}
	if !strings.Contains(string(lastGood), "max_connections = '200'") {
		t.Errorf("last known good = %q, want the set the engine is already running", lastGood)
	}
}

// A probe that reports what the test hands it, in place of whichever binaries
// the engine's own state function would have run.
func mariadbTestProbeConfig(t *testing.T) config {
	t.Helper()
	cfg := testLoadConfig(t, engineMariaDB)
	cfg.EnginePidFile = filepath.Join(t.TempDir(), "mariadb.pid")
	return cfg
}

func writePid(t *testing.T, path string, pid int) {
	t.Helper()
	if err := os.WriteFile(path, []byte(strconv.Itoa(pid)+"\n"), 0o644); err != nil {
		t.Fatalf("write pidfile: %v", err)
	}
}

// The three stages, and what each one separates. During InnoDB crash recovery
// mariadbd opens neither its socket nor its port, so nothing the client can ask
// distinguishes a server replaying its redo log from no server at all.
func TestMariaDBProbe_SeparatesRecoveringFromAbsent(t *testing.T) {
	tests := []struct {
		name     string
		writePid bool
		alive    bool
		pingCode int
		pingErr  error
		execCode int
		want     engineState
	}{
		{name: "no pidfile at all", want: engineAbsent},
		{name: "pidfile names a dead process", writePid: true, want: engineAbsent},
		{name: "process alive but socket silent", writePid: true, alive: true, pingCode: 1, want: engineRecovering},
		{name: "process alive but probe cannot run", writePid: true, alive: true, pingErr: errors.New("no such file"), want: engineRecovering},
		{name: "answers ping but executes nothing", writePid: true, alive: true, execCode: 1, want: engineRecovering},
		{name: "serving", writePid: true, alive: true, want: engineServing},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := mariadbTestProbeConfig(t)
			if tt.writePid {
				writePid(t, cfg.EnginePidFile, 4242)
			}
			run := func(_ context.Context, name string, args ...string) (int, error) {
				if strings.HasSuffix(name, mariadbAdminBinary) {
					return tt.pingCode, tt.pingErr
				}
				if !slices.Contains(args, "--execute=SELECT 1") {
					t.Errorf("the second stage ran %s %v, want a statement the server has to execute", name, args)
				}
				return tt.execCode, nil
			}
			stateFn := mariadbProbeState(cfg, run, func(int) bool { return tt.alive })

			got, message := stateFn(context.Background(), int64(cfg.EnginePort))
			if got != tt.want {
				t.Errorf("state = %v, want %v (%s)", got, tt.want, message)
			}
			if got != engineServing && message == "" {
				t.Error("a non-serving result carried no message explaining it")
			}
		})
	}
}

// The failure the guard exists for still behaves correctly: a static value
// mariadbd will not accept makes it exit during startup, so the pidfile names a
// process that is gone and the deadline runs.
func TestMariaDBProbe_BoundsAStalledClient(t *testing.T) {
	cfg := mariadbTestProbeConfig(t)
	cfg.EngineProbeTimeout = 20 * time.Millisecond
	writePid(t, cfg.EnginePidFile, 4242)

	started := time.Now()
	state, message := mariadbProbeState(cfg,
		func(ctx context.Context, _ string, args ...string) (int, error) {
			if !slices.Contains(args, "--connect-timeout=3") {
				t.Errorf("probe args = %v, want a client connection timeout", args)
			}
			<-ctx.Done()
			return -1, ctx.Err()
		},
		func(int) bool { return true })(t.Context(), int64(cfg.EnginePort))

	if state != engineRecovering {
		t.Errorf("state = %v, want recovering after a live engine's probe timed out", state)
	}
	if !strings.Contains(message, context.DeadlineExceeded.Error()) {
		t.Errorf("message = %q, want the probe deadline", message)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Errorf("probe took %v, want the configured deadline to bound it", elapsed)
	}
}

func TestMariaDBProbe_MapsToTheHealthTheControlPlaneReads(t *testing.T) {
	cfg := mariadbTestProbeConfig(t)
	writePid(t, cfg.EnginePidFile, 4242)
	alive := true
	serving := true
	probe := newEngineProbe(cfg.EnginePort, mariadbProbeState(cfg,
		func(context.Context, string, ...string) (int, error) {
			if serving {
				return 0, nil
			}
			return 1, nil
		},
		func(int) bool { return alive }))

	if got, _ := probe.Check(context.Background()); got != handlers_rds.EngineHealthHealthy {
		t.Fatalf("health = %q, want healthy", got)
	}
	serving = false
	if got, _ := probe.Check(context.Background()); got != handlers_rds.EngineHealthStarting {
		t.Errorf("health while recovering = %q, want starting", got)
	}
	alive = false
	if got, _ := probe.Check(context.Background()); got != handlers_rds.EngineHealthUnhealthy {
		t.Errorf("health after the engine went away = %q, want unhealthy", got)
	}
}

// An instance killed hard mid-write can spend minutes replaying its redo log. If
// that read as absent, the guard would roll the parameter file back and restart
// the server mid-recovery — a rollback that exists to break a boot loop would
// create one, on an instance whose parameters were never at fault.
func TestMariaDBProbe_LiveButUnreachableEngineResetsTheRollbackDeadline(t *testing.T) {
	cfg := mariadbTestProbeConfig(t)
	writePid(t, cfg.EnginePidFile, 4242)
	probe := newEngineProbe(cfg.EnginePort, mariadbProbeState(cfg,
		func(context.Context, string, ...string) (int, error) { return 1, nil },
		func(int) bool { return true }))

	recovery := &fakeRecovery{restored: true}
	guard := newTestGuard(recovery, probe)
	// Bounded so the deadline-resetting loop cannot run forever if it stops
	// resetting; the assertion is that the rollback was never reached.
	ctx, cancel := context.WithTimeout(t.Context(), 200*time.Millisecond)
	defer cancel()
	guard.Run(ctx)

	if recovery.restarts != 0 {
		t.Errorf("restarts = %d, want none while the engine is still recovering", recovery.restarts)
	}
}

// MariaDB releases a BACKUP STAGE with the connection that took it, so the whole
// sequence has to run on one held session — which is also what bounds a control
// plane that dies mid-snapshot.
func TestMariaDBEngine_QuiesceTakesTheBackupStagesOnAHeldSession(t *testing.T) {
	session := &fakeSession{}
	engine := newTestMariaDBEngine(t, (&recordingRunner{}).run)
	var started []command
	engine.startSess = func(_ context.Context, c command) (engineSession, error) {
		started = append(started, c)
		return session, nil
	}

	if err := engine.Quiesce(context.Background(), "orders-db-pre-upgrade", time.Minute); err != nil {
		t.Fatalf("Quiesce: %v", err)
	}

	if len(started) != 1 {
		t.Fatalf("started %d sessions, want 1", len(started))
	}
	if !slices.Contains(started[0].Args, "--unbuffered") {
		t.Errorf("session argv %v does not flush after each statement, so the sentinel could sit in a pipe buffer", started[0].Args)
	}
	statements := session.statements()
	want := []string{
		"SET SESSION lock_wait_timeout = 20;",
		"BACKUP STAGE START;",
		"BACKUP STAGE FLUSH;",
		"BACKUP STAGE BLOCK_DDL;",
		"BACKUP STAGE BLOCK_COMMIT;",
	}
	if len(statements) != len(want) {
		t.Fatalf("statements = %q, want the bounded stage sequence %q", statements, want)
	}
	for i, sql := range want {
		if strings.TrimSpace(statements[i]) != sql {
			t.Errorf("statement %d = %q, want %q", i, statements[i], sql)
		}
	}
	// FLUSH TABLES WITH READ LOCK would make the whole database read-only for the
	// length of the hold rather than blocking commits.
	if strings.Contains(strings.Join(statements, " "), "FLUSH TABLES") {
		t.Error("the quiesce took a read lock over the whole database")
	}
	if session.closes() != 0 {
		t.Error("the session was closed while the backup was meant to be held")
	}
}

// The stage bound has to leave the control plane's own quiesce timeout room for
// the whole sequence, or a stage abandoned mid-wait leaves its lock request
// queued in front of live traffic.
func TestMariaDBEngine_QuiesceLockWaitFitsInsideTheControlPlaneTimeout(t *testing.T) {
	const stages = 4
	if stages*mariadbQuiesceLockWait >= 2*time.Minute {
		t.Errorf("%d stages of %s can outlast the control plane's quiesce timeout", stages, mariadbQuiesceLockWait)
	}
}

func TestMariaDBEngine_UnquiesceEndsTheBackupAndTheSession(t *testing.T) {
	session := &fakeSession{}
	engine := newTestMariaDBEngine(t, (&recordingRunner{}).run)
	engine.startSess = func(context.Context, command) (engineSession, error) { return session, nil }
	if err := engine.Quiesce(context.Background(), "orders-db-pre-upgrade", time.Minute); err != nil {
		t.Fatalf("Quiesce: %v", err)
	}

	if err := engine.Unquiesce(context.Background()); err != nil {
		t.Fatalf("Unquiesce: %v", err)
	}

	statements := session.statements()
	if last := strings.TrimSpace(statements[len(statements)-1]); last != "BACKUP STAGE END;" {
		t.Errorf("last statement = %q, want the stage released", last)
	}
	if session.closes() != 1 {
		t.Errorf("closed %d times, want the session ended exactly once", session.closes())
	}
}

// The two halves of a time_zone apply use different names: SET GLOBAL takes the
// customer's, the option file the server's startup spelling. Getting either
// backwards is a statement the server refuses or a boot it refuses.
func TestMariaDBEngine_TimeZoneAppliesLiveByItsCustomerFacingName(t *testing.T) {
	runner := &recordingRunner{}
	engine := newTestMariaDBEngine(t, runner.run)

	if _, err := engine.ApplyParameters(context.Background(), []handlers_rds.Parameter{
		{Name: "time_zone", Value: "+10:00"},
	}); err != nil {
		t.Fatalf("ApplyParameters: %v", err)
	}

	if len(runner.calls) != 1 {
		t.Fatalf("ran %d commands, want the one live apply", len(runner.calls))
	}
	sql := runner.calls[0].Stdin
	if !strings.Contains(sql, "SET GLOBAL time_zone = '+10:00';") {
		t.Errorf("SQL %q does not set time_zone under the name MariaDB accepts for SET", sql)
	}
	if strings.Contains(sql, "default_time_zone") {
		t.Errorf("SQL %q names the startup spelling, which is not a settable variable", sql)
	}

	installed, err := os.ReadFile(engine.params.installedPath())
	if err != nil {
		t.Fatalf("read the installed parameters: %v", err)
	}
	if !strings.Contains(string(installed), "default_time_zone = '+10:00'") {
		t.Errorf("installed parameters = %q, want the startup spelling", installed)
	}
}

// A dynamic setting applied live deliberately leaves the serving copy alone, so
// the two files differ by design. Read back under the startup spelling the
// catalog does not carry, that difference would classify as static and report a
// pending restart that nothing could ever clear.
func TestMariaDBEngine_ALiveTimeZoneChangeLeavesNothingPendingRestart(t *testing.T) {
	runner := &recordingRunner{}
	engine := newTestMariaDBEngine(t, runner.run)

	if err := os.WriteFile(engine.params.servingPath(),
		[]byte("[mysqld]\ndefault_time_zone = 'SYSTEM'\n"), 0o600); err != nil {
		t.Fatalf("seed the serving copy: %v", err)
	}

	pending, err := engine.ApplyParameters(context.Background(), []handlers_rds.Parameter{
		{Name: "time_zone", Value: "+10:00"},
	})
	if err != nil {
		t.Fatalf("ApplyParameters: %v", err)
	}
	if len(pending) != 0 {
		t.Errorf("pending = %v, want nothing: time_zone is dynamic and was applied live", pending)
	}
}

// The list is customer-facing, so a static setting still has to be reported
// under the name the customer set rather than the engine's startup spelling.
func TestMariaDBEngine_PendingRestartReportsCatalogNames(t *testing.T) {
	runner := &recordingRunner{}
	engine := newTestMariaDBEngine(t, runner.run)

	if err := os.WriteFile(engine.params.servingPath(),
		[]byte("[mysqld]\ninnodb_buffer_pool_size = '536870912'\ndefault_time_zone = 'SYSTEM'\n"), 0o600); err != nil {
		t.Fatalf("seed the serving copy: %v", err)
	}

	pending, err := engine.ApplyParameters(context.Background(), []handlers_rds.Parameter{
		{Name: "innodb_buffer_pool_size", Value: "1073741824"},
		{Name: "time_zone", Value: "SYSTEM"},
	})
	if err != nil {
		t.Fatalf("ApplyParameters: %v", err)
	}
	if !slices.Equal(pending, []string{"innodb_buffer_pool_size"}) {
		t.Errorf("pending = %v, want only the static setting under its catalog name", pending)
	}
}
