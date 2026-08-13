package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
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

// pg_isready is what gives PostgreSQL three states for free: exit 0 is serving,
// exit 1 is a postmaster up and rejecting connections, and anything else is an
// engine that is not there at all. Resolved on PATH, where the client package
// puts it.
const postgresProbeBinary = "pg_isready"

func newPostgresProbe(cfg config, run probeRunner) *engineProbe {
	return newEngineProbe(cfg.EnginePort, postgresProbeState(cfg.EngineHost, run))
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

// The include the resolved parameter set is rendered to, and the copy of the
// last one the engine accepted. Both live beside the data rather than in /etc,
// matching rds-init: a class change boots a fresh root volume, which would
// otherwise revert them.
const (
	postgresParametersFile = "10-rds-parameters.conf"
	// Deliberately not a .conf name: include_dir globs *.conf, so the rollback
	// copy must not be read as a second set of settings.
	postgresLastGoodFile = "10-rds-parameters.last-good"
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

// Installs the resolved set, validates it with the engine's own config parser
// before the engine ever adopts it, and reloads. A value the engine refuses is
// rolled back here rather than left on the data volume, where it would survive
// every VM replace and turn the next restart into a boot loop.
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

	// The check has to run against the file in place, because the engine parses
	// the datadir's own include_dir. The window that leaves is closed from both
	// ends: the rollback below, and the last-known-good restore the agent runs at
	// boot when the engine does not come up.
	restore, err := e.params.install(params)
	if err != nil {
		return nil, err
	}
	if err := e.checkConfig(ctx); err != nil {
		return nil, errors.Join(fmt.Errorf("the engine rejected the parameter set: %w", err), restore())
	}

	state, _ := e.probe.state(ctx)
	switch state {
	case engineAbsent:
		return e.restartOnRepairSetLocked(ctx)
	case engineRecovering:
		return nil, errors.Join(errors.New("the engine entered startup or recovery during the parameter apply"), restore())
	}

	if _, err := e.psqlRun(ctx, "SELECT pg_reload_conf();\n"); err != nil {
		// The engine may have gone down after the first probe. In that case start
		// it on the new, parser-checked repair set rather than restoring the set
		// that already left it unable to serve.
		if state, _ := e.probe.state(ctx); state == engineAbsent {
			return e.restartOnRepairSetLocked(ctx)
		}
		return nil, errors.Join(fmt.Errorf("reload the engine configuration: %w", err), restore())
	}
	return e.pendingRestartParameters(ctx)
}

func (e *postgresEngine) restartOnRepairSetLocked(ctx context.Context) ([]string, error) {
	return awaitRepairedEngine(ctx, e.probe, e.Restart, e.pendingRestartParameters, e.repairTimeout, e.repairPoll)
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
	return restoreLastGood(ctx, e.params, e.probe)
}

// The engine's own parser, run offline against the datadir. Reading one setting
// back is enough: postgres parses postgresql.conf and every include first, and
// exits non-zero naming the file and line of an unknown parameter or a value
// outside its range.
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
