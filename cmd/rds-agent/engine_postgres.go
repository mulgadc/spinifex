package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	handlers_rds "github.com/mulgadc/spinifex/spinifex/handlers/rds"
)

// The engine is reached over its unix socket under peer authentication, so the
// agent drops to the postgres OS user rather than holding a password of its own.
type postgresEngine struct {
	quiesceState
	parameterManager

	// The control plane's own metadata for this engine, resolved once at
	// startup: the live password apply runs as the cluster superuser, so it
	// re-checks the role name against the same reserved set the API validates.
	meta      handlers_rds.Engine
	run       commandRunner
	startSess sessionRunner
	psql      string
	rcService string
	service   string
	pgData    string
	socketDir string
	osUser    string
}

var _ engine = (*postgresEngine)(nil)

// pg_isready gives PostgreSQL three states for free: exit 0 is serving, exit 1
// is a postmaster up and rejecting connections, anything else is an engine that
// is not there. Resolved on PATH, where the client package puts it.
const postgresProbeBinary = "pg_isready"

// The socket directory rather than loopback: the engine binds the customer ENI
// and nothing else, so there is no TCP path for the probe to take. It also keeps
// liveness independent of the network path, of the pg_hba scope and of the TLS
// enforcement rule — libpq reads a host beginning with / as a socket directory,
// and the generated `local ... peer` line covers it.
func newPostgresProbe(cfg config, run probeRunner) *engineProbe {
	return newEngineProbe(cfg.EnginePort, postgresProbeState(cfg.SocketDir, run))
}

func postgresProbeState(host string, run probeRunner) probeStateFn {
	return func(ctx context.Context, port int64) (engineState, string) {
		portArg := strconv.FormatInt(port, 10)
		code, _, err := run(ctx, postgresProbeBinary, "-h", host, "-p", portArg, "-q")
		switch {
		case err != nil:
			// A missing binary or broken image. Reporting healthy on the strength of
			// nothing would hide it, so this reads as absent like an engine that did
			// not answer.
			return engineAbsent, fmt.Sprintf("engine probe could not run: %v", err)
		case code == 0:
			return engineServing, ""
		case code == 1:
			return engineRecovering, "engine is rejecting connections (startup or recovery)"
		default:
			return engineAbsent, fmt.Sprintf("engine did not respond on %s:%s", host, portArg)
		}
	}
}

// Resolved once, under the name the control plane knows this implementation by.
// A lookup failure is a mismatch between the agent and the control plane it was
// built against, so it fails at startup rather than at the first rotation.
var postgresEngineMeta = mustLookupEngine(enginePostgres)

func mustLookupEngine(name string) handlers_rds.Engine {
	engine, err := handlers_rds.LookupEngine(name)
	if err != nil {
		panic("rds-agent: " + err.Error())
	}
	return engine
}

// The include the resolved set is rendered to, and the copy of the last one the
// engine accepted. Both live beside the data rather than in /etc, matching
// rds-init: a class change boots a fresh root volume and would revert them.
const (
	postgresParametersFile = "10-rds-parameters.conf"
	// Deliberately not a .conf name: include_dir globs *.conf, so the rollback
	// copy must not be read as a second set of settings.
	postgresLastGoodFile = "10-rds-parameters.last-good"
)

// The pg_hba rule that implements TLS enforcement, and the directory the
// generated pg_hba includes it from. Its content is a constant, so the copy
// rds-init writes in shell and the one written here have no template to drift
// apart on: enforcement is the presence of the file and nothing more.
const (
	postgresHBADir           = "hba.d"
	postgresForceSSLRuleFile = "20-rds-force-ssl.conf"
	// An explicit reject rather than an omitted permit: both refuse the
	// connection, and only this one puts the reason in the server log and in the
	// client's error.
	postgresForceSSLRules = `# Managed by the platform: the rule that requires TLS of client connections.
# Written by rds-init at boot and by rds-agent on a parameter apply.
hostnossl all all 0.0.0.0/0 reject
hostnossl all all ::/0 reject
`
	// What the read-back must count once the file is in place. Asserting the shape
	// rather than only the absence of parse errors is what catches the failure
	// that will actually happen: the derivation above writing the wrong thing.
	postgresForceSSLRuleCount = 2
)

func newPostgresEngine(cfg config, run commandRunner, startSess sessionRunner, probe *engineProbe) *postgresEngine {
	return &postgresEngine{
		meta:      postgresEngineMeta,
		run:       run,
		startSess: startSess,
		psql:      filepath.Join(cfg.EngineBinDir, "psql"),
		rcService: cfg.RCService,
		service:   cfg.EngineService,
		pgData:    cfg.EngineDataDir,
		socketDir: cfg.SocketDir,
		osUser:    cfg.EngineUser,
		parameterManager: parameterManager{
			probe: probe,
			params: parameterStore{
				dir:       filepath.Join(cfg.EngineDataDir, "conf.d"),
				installed: postgresParametersFile,
				lastGood:  postgresLastGoodFile,
				osUser:    cfg.EngineUser,
				engine:    enginePostgres,
			},
			repairTimeout: parameterRepairTimeout,
			repairPoll:    parameterRepairPoll,
		},
	}
}

// The role name and the password ride the environment and are re-quoted by
// psql, so neither reaches a shell word or an argv another process can read.
func (e *postgresEngine) SetPassword(ctx context.Context, username, password string) error {
	if username == "" || password == "" {
		return fmt.Errorf("set-password requires both %s and %s",
			handlers_rds.CommandParamMasterUsername, handlers_rds.CommandParamMasterUserPassword)
	}
	// The statement below runs as the cluster superuser over the socket under
	// peer auth, so a reserved name in the command payload would hand the
	// customer the bootstrap superuser rather than rotate their own role.
	if err := e.meta.ValidateUsernameNotReserved(username); err != nil {
		return fmt.Errorf("refusing to set the password of a role the engine reserves: %w", err)
	}
	// psql interpolates the password into the ALTER ROLE before the server sees
	// it, and these three are what would write it to the log — the last on any
	// failure at its own default. SUSET, so the parameter group cannot win.
	const sql = `SET log_statement = 'none';
SET log_min_duration_statement = -1;
SET log_min_error_statement = 'panic';
\getenv master RDS_MASTER_USERNAME
\getenv password RDS_MASTER_PASSWORD
ALTER ROLE :"master" WITH LOGIN PASSWORD :'password';
`
	_, err := e.psqlRun(ctx, sql,
		"RDS_MASTER_USERNAME="+username,
		"RDS_MASTER_PASSWORD="+password,
	)
	if err != nil {
		// psql echoes the failing statement, which here would carry the new
		// password back to the control plane and into the event ring.
		return fmt.Errorf("apply the master password: %s", redact(err.Error(), password))
	}
	return nil
}

// Installs the resolved set, validates it with the engine's own parser before
// the engine adopts it, and reloads. A refused value is rolled back here rather
// than left on the data volume, where it would survive every VM replace.
func (e *postgresEngine) ApplyParameters(ctx context.Context, params []handlers_rds.Parameter) ([]string, error) {
	e.paramMu.Lock()
	defer e.paramMu.Unlock()

	initialState, _ := e.probe.state(ctx)
	if initialState == engineRecovering {
		return nil, errors.New("apply parameters while the engine is still starting or recovering")
	}
	if initialState == engineServing {
		// A command can arrive before the first heartbeat. Seed the configuration
		// currently being served before replacing its file in that window.
		if err := e.recordServingParametersLocked(ctx); err != nil {
			return nil, fmt.Errorf("record the parameters serving before the apply: %w", err)
		}
	}

	// The check runs against the file in place, because the engine parses the
	// datadir's own include_dir. The window that leaves is closed by the rollback
	// below and the last-known-good restore the agent runs at boot.
	restoreParameters, err := e.params.install(params)
	if err != nil {
		return nil, err
	}
	if err := e.checkConfig(ctx); err != nil {
		return nil, errors.Join(fmt.Errorf("the engine rejected the parameter set: %w", err), restoreParameters())
	}

	state, _ := e.probe.state(ctx)
	switch state {
	case engineAbsent:
		return e.restartOnRepairSetLocked(ctx)
	case engineRecovering:
		return nil, errors.Join(errors.New("the engine entered startup or recovery during the parameter apply"), restoreParameters())
	}

	// After the engine's own parser has passed the set and before the reload that
	// adopts both files at once.
	enforce, restoreEnforcement, err := e.syncTLSEnforcement(ctx, state)
	if err != nil {
		return nil, errors.Join(fmt.Errorf("apply the TLS enforcement of the parameter set: %w", err), restoreParameters())
	}
	// The two files carry one apply from here: a rejected set withdrawn without
	// its enforcement rule would leave the instance serving the old parameters
	// under the new set's TLS posture.
	restore := func() error { return errors.Join(restoreEnforcement(), restoreParameters()) }

	if err := e.reload(ctx); err != nil {
		// The engine may have gone down after the first probe. In that case start
		// it on the new, parser-checked repair set rather than restoring the set
		// that already left it unable to serve.
		if state, _ := e.probe.state(ctx); state == engineAbsent {
			return e.restartOnRepairSetLocked(ctx)
		}
		return nil, errors.Join(err, restore())
	}
	if err := e.verifyTLSEnforcement(ctx, enforce); err != nil {
		// The reload already happened, so putting the files back is not enough: a
		// pg_hba that parsed but says the wrong thing has been adopted, and only a
		// second reload takes the engine off it.
		return nil, errors.Join(err, restore(), e.reload(ctx))
	}
	return e.pendingRestartParameters(ctx)
}

// Re-derived before the restart: the engine reads pg_hba when it starts, so a
// repair set started under the previous set's enforcement would serve one TLS
// posture while the API reported the other.
func (e *postgresEngine) restartOnRepairSetLocked(ctx context.Context) ([]string, error) {
	if _, _, err := e.syncTLSEnforcement(ctx, engineAbsent); err != nil {
		return nil, fmt.Errorf("apply the TLS enforcement of the repair set: %w", err)
	}
	return awaitRepairedEngine(ctx, e.probe, e.Restart, e.pendingRestartParameters, e.repairTimeout, e.repairPoll)
}

// The function's own answer is the reload, not psql's exit status: it returns f
// when it could not signal the postmaster, and the read-back that follows parses
// pg_hba from disk, so an unsignalled reload would otherwise verify clean.
func (e *postgresEngine) reload(ctx context.Context) error {
	out, err := e.psqlRun(ctx, "SELECT pg_reload_conf();\n")
	if err != nil {
		return fmt.Errorf("reload the engine configuration: %w", err)
	}
	if result := strings.TrimSpace(out); result != "t" {
		return fmt.Errorf("reload the engine configuration: pg_reload_conf() returned %q", result)
	}
	return nil
}

// The postmaster's own answer: a static setting it has stored but not adopted is
// what it reports here, so nothing in the guest has to classify one.
func (e *postgresEngine) pendingRestartParameters(ctx context.Context) ([]string, error) {
	out, err := e.psqlRun(ctx, "SELECT name FROM pg_settings WHERE pending_restart ORDER BY name;\n")
	if err != nil {
		return nil, fmt.Errorf("read the settings pending a restart: %w", err)
	}
	var pending []string
	for line := range strings.SplitSeq(out, "\n") {
		if name := strings.TrimSpace(line); name != "" {
			pending = append(pending, name)
		}
	}
	return pending, nil
}

// Snapshots the include only when the running postmaster has no settings still
// pending a restart. The parameter mutex keeps an apply from replacing the file
// between that check and the copy.
func (e *postgresEngine) RecordServingParameters(ctx context.Context) error {
	e.paramMu.Lock()
	defer e.paramMu.Unlock()
	return e.recordServingParametersLocked(ctx)
}

func (e *postgresEngine) recordServingParametersLocked(ctx context.Context) error {
	return recordLastGood(ctx, e.params, e.pendingRestartParameters)
}

// Puts the last set the engine accepted back in place, for a restart that failed
// after a parameter change.
func (e *postgresEngine) RestoreLastKnownGoodParameters(ctx context.Context) (bool, error) {
	e.paramMu.Lock()
	defer e.paramMu.Unlock()
	restored, err := restoreLastGood(ctx, e.params, e.probe)
	if err != nil || !restored {
		return restored, err
	}
	// A service restart does not re-run rds-init, so a restore that stopped at the
	// parameter file would bring the last known good set up under the enforcement
	// of the set that was just rejected. restoreLastGood only replaces the file of
	// an engine that is down.
	if _, _, err := e.syncTLSEnforcement(ctx, engineAbsent); err != nil {
		return true, fmt.Errorf("apply the TLS enforcement of the restored parameter set: %w", err)
	}
	return true, nil
}

// Turns the installed enforcement value into the pg_hba fact that implements it.
// PostgreSQL has no server setting for this, so the parameter file on its own
// would report the change applied and enforce nothing.
//
// The serving-TLS guard runs only against an engine that is up. The restore and
// repair paths reach this with the engine down, where the rule is read by the
// start that follows rather than by a reload.
func (e *postgresEngine) syncTLSEnforcement(ctx context.Context, state engineState) (enforce bool, restore func() error, err error) {
	enforce, err = installedTLSEnforcement(e.meta.TLSEnforcementParameter(), e.params.installedPath())
	if err != nil {
		return false, nil, err
	}
	if enforce && state == engineServing {
		if err := e.requireServingTLS(ctx); err != nil {
			return false, nil, err
		}
	}
	restore, err = e.writeTLSEnforcement(enforce)
	if err != nil {
		return false, nil, err
	}
	return enforce, restore, nil
}

// Writes or removes the enforcement rule, returning a func putting back whatever
// was there. Removing matters as much as writing: a restored data volume carries
// hba.d with it, so a stale rule would keep enforcing under a parameter group
// that turns enforcement off.
func (e *postgresEngine) writeTLSEnforcement(enforce bool) (func() error, error) {
	path := e.forceSSLRulePath()
	previous, readErr := os.ReadFile(path)
	if readErr != nil && !os.IsNotExist(readErr) {
		return nil, fmt.Errorf("read the TLS enforcement rule at %s: %w", path, readErr)
	}
	existed := readErr == nil
	restore := func() error {
		if !existed {
			return e.removeTLSEnforcement()
		}
		return e.params.write(path, previous)
	}

	if !enforce {
		if err := e.removeTLSEnforcement(); err != nil {
			return nil, err
		}
		return restore, nil
	}
	// rds-init lays the directory down on every boot, so this covers an agent that
	// reached an apply before one ran rather than the ordinary case.
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("create %s: %w", dir, err)
	}
	if err := e.params.chownToEngine(dir); err != nil {
		return nil, err
	}
	if err := e.params.write(path, []byte(postgresForceSSLRules)); err != nil {
		return nil, err
	}
	return restore, nil
}

func (e *postgresEngine) removeTLSEnforcement() error {
	path := e.forceSSLRulePath()
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("withdraw the TLS enforcement rule at %s: %w", path, err)
	}
	return nil
}

func (e *postgresEngine) forceSSLRulePath() string {
	return filepath.Join(e.pgData, postgresHBADir, postgresForceSSLRuleFile)
}

// Refuses to require TLS of clients from an engine that is not serving it, which
// would reject every connection while the parameter path reported success.
func (e *postgresEngine) requireServingTLS(ctx context.Context) error {
	out, err := e.psqlRun(ctx, "SHOW ssl;\n")
	if err != nil {
		return fmt.Errorf("read whether the engine is serving TLS: %w", err)
	}
	if serving := strings.TrimSpace(out); serving != "on" {
		return fmt.Errorf("the engine is serving without TLS (ssl = %q), so requiring it of clients would reject every connection", serving)
	}
	return nil
}

// PostgreSQL discards a pg_hba it cannot parse on reload and keeps the rules it
// is already serving, which presents as an apply that succeeded and enforces
// nothing. So the file is read back.
//
// pg_hba_file_rules re-parses from disk, so this proves the file would be
// adopted rather than that the postmaster has adopted it. The inference holds
// because a reloaded pg_hba is discarded only on a parse error.
const postgresHBAReadBack = `SELECT count(*) FILTER (WHERE type = 'hostnossl' AND auth_method = 'reject'),
       count(*) FILTER (WHERE error IS NOT NULL) FROM pg_hba_file_rules;
`

func (e *postgresEngine) verifyTLSEnforcement(ctx context.Context, enforce bool) error {
	out, err := e.psqlRun(ctx, postgresHBAReadBack)
	if err != nil {
		return fmt.Errorf("read back the client authentication rules: %w", err)
	}
	rules, unparsable, err := parseHBAReadBack(out)
	if err != nil {
		return err
	}
	if unparsable > 0 {
		return fmt.Errorf("the client authentication file carries %d rules the engine could not parse, so it would keep the rules it is already serving", unparsable)
	}
	want := 0
	if enforce {
		want = postgresForceSSLRuleCount
	}
	if rules != want {
		return fmt.Errorf("the client authentication file carries %d TLS enforcement rules, want %d", rules, want)
	}
	return nil
}

func parseHBAReadBack(out string) (rules, unparsable int, err error) {
	answer := strings.TrimSpace(out)
	malformed := fmt.Errorf("the client authentication read-back returned %q, want two counts", answer)
	counts := strings.Split(answer, "|")
	if len(counts) != 2 {
		return 0, 0, malformed
	}
	parsed := make([]int, len(counts))
	for i, count := range counts {
		if parsed[i], err = strconv.Atoi(strings.TrimSpace(count)); err != nil {
			return 0, 0, malformed
		}
	}
	return parsed[0], parsed[1], nil
}

// The engine's own parser, run offline against the datadir. Reading one setting
// back is enough: postgres parses every include first and exits non-zero naming
// the file and line of an unknown parameter or an out-of-range value.
func (e *postgresEngine) checkConfig(ctx context.Context) error {
	if _, err := e.run(ctx, command{
		Name: filepath.Join(filepath.Dir(e.psql), "postgres"),
		Args: []string{"-D", e.pgData, "-C", "shared_buffers"},
		Env:  []string{"PATH=" + defaultGuestPath},
		User: e.osUser,
	}); err != nil {
		return err
	}
	return nil
}

func (e *postgresEngine) Stop(ctx context.Context) error {
	return serviceAction(ctx, e.run, e.rcService, e.service, "stop")
}

// Only the parameter rollback calls this: a restart the control plane wants goes
// through RebootDBInstance, which cycles the VM.
func (e *postgresEngine) Restart(ctx context.Context) error {
	return serviceAction(ctx, e.run, e.rcService, e.service, "restart")
}

// Feeds sql on stdin rather than as an argument, so a statement is never
// visible in the process table.
func (e *postgresEngine) psqlRun(ctx context.Context, sql string, env ...string) (string, error) {
	return e.run(ctx, command{
		Name:  e.psql,
		Args:  e.psqlArgs(),
		Env:   append([]string{"PATH=" + defaultGuestPath}, env...),
		Stdin: sql,
		User:  e.osUser,
	})
}

// Reading the script from stdin is what lets one invocation serve both a
// one-shot run and a session held open across several statements.
func (e *postgresEngine) psqlArgs() []string {
	return []string{
		"--no-psqlrc", "--quiet", "--no-align", "--tuples-only",
		"-v", "ON_ERROR_STOP=1",
		"-h", e.socketDir,
		"-p", strconv.FormatInt(e.probe.port.Load(), 10),
		"-U", e.osUser,
		"-d", "postgres",
		"-f", "-",
	}
}

// Puts the engine into backup mode: the datadir is checkpointed and the engine
// stops writing over the pages a snapshot is about to read. The hold is released
// by Unquiesce, or by its own deadline, whichever comes first.
func (e *postgresEngine) Quiesce(ctx context.Context, label string, hold time.Duration) error {
	if err := validateQuiesceRequest(label, hold); err != nil {
		return err
	}

	// Held across the whole start, so a second quiesce waits and then finds the
	// first one's hold rather than opening a concurrent backup alongside it.
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.held != nil {
		return fmt.Errorf("the engine is already quiesced for backup %s", e.held.label)
	}

	// Deliberately not the command's context: the session has to outlive the call
	// that started it, and its own deadline is what bounds it instead.
	session, err := e.startSess(context.WithoutCancel(ctx), command{
		Name:              e.psql,
		Args:              e.psqlArgs(),
		Env:               []string{"PATH=" + defaultGuestPath, "RDS_BACKUP_LABEL=" + label},
		User:              e.osUser,
		SentinelStatement: `\echo ` + sessionSentinel + "\n",
	})
	if err != nil {
		return fmt.Errorf("open a backup session: %w", err)
	}

	// fast forces an immediate checkpoint rather than spreading it over the
	// checkpoint interval, which would hold the snapshot open for minutes.
	const sql = `\getenv label RDS_BACKUP_LABEL
SELECT pg_backup_start(:'label', fast => true);
`
	if err := session.Exec(ctx, sql); err != nil {
		if closeErr := session.Close(); closeErr != nil {
			slog.Warn("rds-agent: closing a failed backup session", "err", closeErr)
		}
		return fmt.Errorf("put the engine into backup mode: %w", err)
	}

	e.beginHoldLocked(label, session, hold)
	return nil
}

func (e *postgresEngine) Unquiesce(ctx context.Context) error {
	return e.releaseHold(ctx, "SELECT pg_backup_stop();\n")
}
