package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	handlers_rds "github.com/mulgadc/spinifex/spinifex/handlers/rds"
)

// The control plane's own statements about this engine, resolved once at
// startup. Held as functions rather than as the Engine value so nothing here
// re-derives a rule the API already owns.
type controlPlaneRules struct {
	// The whole master-username rule rather than only the reserved set. MariaDB's
	// client has no identifier quoting, so the name is interpolated into SQL by
	// this process and every part of the rule is load-bearing here.
	validateUsername func(username string) error
	// Whether a setting takes effect only at a restart. Already customer-facing
	// and authoritative, since the API refuses ApplyMethod=immediate on one.
	isStatic func(name string) bool
	// The catalog name behind a spelling read back out of an option file. Only a
	// setting whose startup spelling differs from the customer's moves.
	catalogName func(optionFileName string) string
}

func controlPlaneRulesFrom(meta handlers_rds.Engine) controlPlaneRules {
	// Built once per engine: everything read back out of a generated file has to
	// return to the catalog's namespace before it is classified or reported, or a
	// startup spelling would read as an unknown name.
	catalogNames := map[string]string{}
	for _, name := range meta.CatalogParameterNames() {
		if optionFileName := meta.OptionFileName(name); optionFileName != name {
			catalogNames[optionFileName] = name
		}
	}

	return controlPlaneRules{
		validateUsername: meta.ValidateMasterUsername,
		isStatic: func(name string) bool {
			spec, ok := meta.LookupParameter(name)
			// A name the catalog does not carry cannot be shown to have been adopted
			// without a restart, and is never issued as a live SET GLOBAL either.
			return !ok || spec.ApplyType == handlers_rds.ApplyTypeStatic
		},
		catalogName: func(optionFileName string) string {
			if name, ok := catalogNames[optionFileName]; ok {
				return name
			}
			return optionFileName
		},
	}
}

// The engine is reached over its unix socket as root, which the datadir's
// unix_socket plugin authenticates from the connecting process' own uid — the
// direct analogue of PostgreSQL's peer auth, and the reason the agent holds no
// password of its own.
type mariadbEngine struct {
	quiesceState

	rules     controlPlaneRules
	run       commandRunner
	startSess sessionRunner
	client    string
	admin     string
	socket    string
	rcService string
	service   string
	probe     *engineProbe

	params parameterStore
	// Serializes parameter installs, serving snapshots and rollback restores so
	// none can copy or replace an intermediate configuration.
	paramMu       sync.Mutex
	repairTimeout time.Duration
	repairPoll    time.Duration
}

var _ engine = (*mariadbEngine)(nil)

const (
	mariadbClientBinary = "mariadb"
	mariadbAdminBinary  = "mariadb-admin"
	// The account mariadb-install-db creates for the unix_socket plugin, which
	// rds-init keeps for the platform and never hands to the customer.
	mariadbSuperuser                  = "root"
	mariadbProbeConnectTimeoutSeconds = 3
	// Named by the platform drop-in rds-init writes, so both halves reach the
	// same socket without either asserting it to the other.
	mariadbSocketFile = "mysqld.sock"
	// The customer reaches the instance over its customer ENI, so the master is
	// created and rotated on the wildcard host rather than on localhost.
	mariadbMasterHost = "%"
)

// The generated drop-ins on the data volume. The rollback and serving copies
// deliberately do not end in .cnf: !includedir reads *.cnf, and neither is a
// second set of settings.
const (
	mariadbParametersFile = "10-rds-parameters.cnf"
	mariadbLastGoodFile   = "10-rds-parameters.last-good"
	mariadbServingFile    = "10-rds-parameters.serving"
	// MariaDB treats a setting before any group as a fatal parsing error, so
	// every generated file carries the header rds-init writes.
	mariadbParametersHeader = "[mysqld]\n"
)

// The layout's factory. The rules come from the control plane's own definition
// of this engine, so a build whose control plane does not offer MariaDB refuses
// to run this implementation rather than inventing a definition for it.
func newMariaDBEngineFromCatalog(cfg config, run commandRunner, startSess sessionRunner, probe *engineProbe) (engine, error) {
	meta, err := handlers_rds.LookupEngine(engineMariaDB)
	if err != nil {
		return nil, fmt.Errorf("this image bakes %s, which this build's control plane does not offer: %w", engineMariaDB, err)
	}
	return newMariaDBEngine(cfg, controlPlaneRulesFrom(meta), run, startSess, probe), nil
}

func newMariaDBEngine(cfg config, rules controlPlaneRules, run commandRunner, startSess sessionRunner, probe *engineProbe) *mariadbEngine {
	return &mariadbEngine{
		rules:     rules,
		run:       run,
		startSess: startSess,
		client:    filepath.Join(cfg.EngineBinDir, mariadbClientBinary),
		admin:     filepath.Join(cfg.EngineBinDir, mariadbAdminBinary),
		socket:    mariadbSocketPath(cfg),
		rcService: cfg.RCService,
		service:   cfg.EngineService,
		probe:     probe,
		params: parameterStore{
			// The mount point rather than the datadir one level inside it: the
			// include directory has to outlive the sweep a failed bootstrap runs.
			dir:       filepath.Join(cfg.DataMount, "conf.d"),
			installed: mariadbParametersFile,
			lastGood:  mariadbLastGoodFile,
			serving:   mariadbServingFile,
			header:    mariadbParametersHeader,
			osUser:    cfg.EngineUser,
			engine:    engineMariaDB,
		},
		repairTimeout: parameterRepairTimeout,
		repairPoll:    parameterRepairPoll,
	}
}

func mariadbSocketPath(cfg config) string {
	return filepath.Join(cfg.SocketDir, mariadbSocketFile)
}

// Nothing this implementation runs needs the assigned port: every connection it
// makes is over the engine's unix socket, and the platform drop-in rds-init
// writes is what puts the server on the port. The probe follows it separately.
func (e *mariadbEngine) setPort(int) {}

// Reports whether a pid is a live process. EPERM is a process this agent may not
// signal rather than one that is gone, which matters because a stale pidfile
// read as "alive" is far less dangerous than a live engine read as "absent".
type processLivenessFn func(pid int) bool

func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}

func newMariaDBProbe(cfg config, run probeRunner) *engineProbe {
	return newEngineProbe(cfg.EnginePort, mariadbProbeState(cfg, run, processAlive))
}

// Three stages, because MariaDB has no single signal that separates an engine
// still coming up from one that is not there at all: during InnoDB crash
// recovery mariadbd opens neither its socket nor its port, so a ping fails
// exactly as it would against nothing.
//
// Losing that distinction would break the rollback guard, which treats a
// recovering engine as a reason to reset its deadline. An instance killed hard
// mid-write can spend minutes replaying its redo log; read as absent, the guard
// would roll the parameter file back and restart the server mid-recovery,
// discarding the work — a rollback that exists to break a boot loop would create
// one, on an instance whose parameters were never at fault.
//
// The port is not consulted. Both stages below reach the server over its unix
// socket, whose path does not move with the port.
func mariadbProbeState(cfg config, run probeRunner, alive processLivenessFn) probeStateFn {
	pidFile, socket := cfg.EnginePidFile, mariadbSocketPath(cfg)
	admin, client := filepath.Join(cfg.EngineBinDir, mariadbAdminBinary), filepath.Join(cfg.EngineBinDir, mariadbClientBinary)
	connect := []string{
		"--no-defaults", "--protocol=socket", "--socket=" + socket,
		"--user=" + mariadbSuperuser,
		fmt.Sprintf("--connect-timeout=%d", mariadbProbeConnectTimeoutSeconds),
	}
	probeTimeout := cfg.EngineProbeTimeout
	if probeTimeout <= 0 {
		probeTimeout = defaultEngineProbeTimeout
	}
	runBounded := func(ctx context.Context, name string, args ...string) (int, error) {
		probeCtx, cancel := context.WithTimeout(ctx, probeTimeout)
		defer cancel()
		return run(probeCtx, name, args...)
	}

	return func(ctx context.Context, _ int64) (engineState, string) {
		pid, err := readPidFile(pidFile)
		switch {
		case os.IsNotExist(err):
			return engineAbsent, fmt.Sprintf("the engine has written no pidfile at %s", pidFile)
		case err != nil:
			return engineAbsent, fmt.Sprintf("the engine pidfile %s could not be read: %v", pidFile, err)
		case !alive(pid):
			return engineAbsent, fmt.Sprintf("the engine pidfile %s names pid %d, which is not running", pidFile, pid)
		}

		// The process is up from here on, so every remaining failure is an engine
		// that is not serving yet rather than one that is gone — including a probe
		// binary that will not run at all. Reporting absent against a live server
		// would have the rollback guard restart one that is making progress.
		switch code, err := runBounded(ctx, admin, append(slices.Clone(connect), "ping")...); {
		case err != nil:
			return engineRecovering, fmt.Sprintf("engine probe could not run: %v", err)
		case code != 0:
			return engineRecovering, "engine is not answering on its socket yet (startup or crash recovery)"
		}

		// ping answers successfully even on ER_ACCESS_DENIED, so on its own it
		// certifies a server that may be unable to execute anything at all. The
		// statement is a literal in the argv rather than on stdin because it
		// carries nothing secret and a probe reads no result back.
		query := append(slices.Clone(connect), "--batch", "--skip-column-names", "--execute=SELECT 1")
		switch code, err := runBounded(ctx, client, query...); {
		case err != nil:
			return engineRecovering, fmt.Sprintf("engine probe could not run: %v", err)
		case code != 0:
			return engineRecovering, "engine answered its socket but would not execute a statement"
		}
		return engineServing, ""
	}
}

func readPidFile(path string) (int, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil {
		return 0, fmt.Errorf("%s does not hold a pid: %w", path, err)
	}
	return pid, nil
}

func (e *mariadbEngine) Stop(ctx context.Context) error {
	return serviceAction(ctx, e.run, e.rcService, e.service, "stop")
}

// Only the parameter rollback and a failed apply call this: a restart the
// control plane wants goes through RebootDBInstance, which cycles the VM.
//
// The serving copy is refreshed first, because the server is about to start on
// whatever is installed. That is exactly what rds-init does on every boot, and
// it is what keeps the two equal by construction whenever the engine starts.
func (e *mariadbEngine) Restart(ctx context.Context) error {
	if err := e.recordServingCopy(); err != nil {
		return err
	}
	return serviceAction(ctx, e.run, e.rcService, e.service, "restart")
}

// The installed set copied to the name the pending-restart comparison reads. A
// live SET GLOBAL apply deliberately does not touch it: that value was adopted
// without a restart, which is what "not pending" means.
func (e *mariadbEngine) recordServingCopy() error {
	content, err := os.ReadFile(e.params.installedPath())
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("read the installed parameters: %w", err)
	}
	return e.params.write(e.params.servingPath(), content)
}

// A value safe to interpolate into a single-quoted SQL literal. The quote is
// doubled, which holds in every sql_mode, and the backslash is doubled, which is
// why every statement built here pins a mode with backslash escapes on.
func sqlLiteral(value string) string {
	return strings.ReplaceAll(strings.ReplaceAll(value, `\`, `\\`), `'`, `''`)
}

// Turns off the two logs that would otherwise copy a statement's text, and pins
// the mode the escaping above assumes. All three are session-scoped: a rotation
// must not leave the customer's own general log switched off behind it.
const mariadbSessionGuard = `SET SESSION sql_log_off = 1;
SET SESSION slow_query_log = 0;
SET SESSION sql_mode = 'NO_ENGINE_SUBSTITUTION';
`

// Rotates the master's password. The statement is built here rather than by the
// client, which has neither psql's identifier interpolation nor its parameter
// quoting, and it rides stdin so it is never visible in the process table.
func (e *mariadbEngine) SetPassword(ctx context.Context, username, password string) error {
	if username == "" || password == "" {
		return fmt.Errorf("set-password requires both %s and %s",
			handlers_rds.CommandParamMasterUsername, handlers_rds.CommandParamMasterUserPassword)
	}
	// This runs as the server's own superuser over the socket, so a reserved name
	// in the command payload would rotate the platform's account rather than the
	// customer's. The rest of the rule matters just as much here, because the name
	// is interpolated into the statement below rather than quoted by the client.
	if err := e.rules.validateUsername(username); err != nil {
		return fmt.Errorf("refusing to set the password of role %q: %w", username, err)
	}

	escaped := sqlLiteral(password)
	sql := fmt.Sprintf("%sALTER USER '%s'@'%s' IDENTIFIED BY '%s';\n",
		mariadbSessionGuard, username, mariadbMasterHost, escaped)
	if _, err := e.clientRun(ctx, sql); err != nil {
		// The client echoes the failing statement, which carries the escaped form
		// of the password rather than the raw one — so redacting only the raw one
		// would leak exactly the passwords that needed escaping.
		return fmt.Errorf("apply the master password: %s", redact(redact(err.Error(), password), escaped))
	}
	return nil
}

// Installs the resolved set and applies the half of it a running server can
// adopt. MariaDB re-reads no configuration file while it is up, so SET GLOBAL is
// the only way an immediate apply reaches it; the static half waits in the
// installed file for the next start, and is what comes back as pending.
//
// There is no offline parser to check the set with first — MariaDB ships no
// equivalent of postgres -C — so the safety net for a static value the server
// will not start on is the boot-time rollback to the last accepted set.
func (e *mariadbEngine) ApplyParameters(ctx context.Context, params []handlers_rds.Parameter) ([]string, error) {
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

	restore, err := e.params.install(params)
	if err != nil {
		return nil, err
	}

	state, _ := e.probe.state(ctx)
	switch state {
	case engineAbsent:
		return e.restartOnRepairSetLocked(ctx)
	case engineRecovering:
		return nil, errors.Join(errors.New("the engine entered startup or recovery during the parameter apply"), restore())
	}

	if err := e.applyDynamicParameters(ctx, params); err != nil {
		// The engine may have gone down after the first probe. In that case start it
		// on the newly installed set rather than restoring one that already left it
		// unable to serve.
		if state, _ := e.probe.state(ctx); state == engineAbsent {
			return e.restartOnRepairSetLocked(ctx)
		}
		return nil, errors.Join(err, restore())
	}
	return e.pendingRestartParameters(ctx)
}

// The dynamic half of the set, in one invocation that stops at the first refusal
// so the server's own message names the setting it would not take.
//
// Only names the catalog carries are emitted, which is also what keeps the
// statement safe to build: a name is an identifier here, and the classification
// that selects it comes from the same table the API validated the request
// against. MariaDB cannot set several globals atomically, so a value set before
// a refusal stays live until the next restart, when the restored file wins.
func (e *mariadbEngine) applyDynamicParameters(ctx context.Context, params []handlers_rds.Parameter) error {
	var b strings.Builder
	b.WriteString("SET SESSION sql_mode = 'NO_ENGINE_SUBSTITUTION';\n")
	applied := 0
	for _, p := range params {
		if p.Name == "" || e.rules.isStatic(p.Name) {
			continue
		}
		fmt.Fprintf(&b, "SET GLOBAL %s = '%s';\n", p.Name, sqlLiteral(p.Value))
		applied++
	}
	if applied == 0 {
		return nil
	}
	if _, err := e.clientRun(ctx, b.String()); err != nil {
		return fmt.Errorf("apply the dynamic parameters: %w", err)
	}
	return nil
}

func (e *mariadbEngine) restartOnRepairSetLocked(ctx context.Context) ([]string, error) {
	return awaitRepairedEngine(ctx, e.probe, e.Restart, e.pendingRestartParameters, e.repairTimeout, e.repairPoll)
}

// MariaDB has no pending_restart. Nothing in information_schema reports a
// setting the server has stored but not adopted, so the answer is computed from
// the files instead: the catalog-static keys whose value in the installed
// drop-in differs from the one the server actually started on.
//
// Comparing live values against the desired set was rejected. mysqld silently
// rewrites values at startup — rounding the buffer pool and log file sizes, and
// downgrading max_connections when it cannot obtain enough file descriptors —
// and each rewrite would read as a permanent pending restart: un-clearable
// pending-reboot state, permanent drift, and a last-known-good copy frozen for
// good, silently disabling a recovery mechanism only exercised during an
// incident.
func (e *mariadbEngine) pendingRestartParameters(context.Context) ([]string, error) {
	installed, err := readOptionFile(e.params.installedPath())
	if err != nil {
		if os.IsNotExist(err) {
			// Nothing is installed, so nothing is waiting to be adopted.
			return nil, nil
		}
		return nil, fmt.Errorf("read the installed parameters: %w", err)
	}
	serving, err := readOptionFile(e.params.servingPath())
	if err != nil && !os.IsNotExist(err) {
		return nil, fmt.Errorf("read the parameters the engine started on: %w", err)
	}
	// Back to the catalog's names before anything is classified or reported: the
	// files are written in the engine's startup spellings, and the answer is a
	// customer-facing list the API refuses ApplyMethod=immediate against.
	installed = e.catalogKeyed(installed)
	serving = e.catalogKeyed(serving)

	// An absent serving copy is a set the engine has not started on: rds-init
	// writes the two together on every boot, so the whole static half counts as
	// pending rather than being promoted on the strength of a missing file.
	var pending []string
	for name, value := range installed {
		if !e.rules.isStatic(name) {
			continue
		}
		if served, ok := serving[name]; !ok || served != value {
			pending = append(pending, name)
		}
	}
	// A static setting the group stopped naming reverts to its default at the next
	// start, which the running server has not adopted either.
	for name := range serving {
		if _, ok := installed[name]; !ok && e.rules.isStatic(name) {
			pending = append(pending, name)
		}
	}
	slices.Sort(pending)
	return pending, nil
}

// Snapshots the installed set as the rollback target, but only when nothing in
// it is still waiting for a restart. The parameter mutex keeps an apply from
// replacing the file between that check and the copy.
func (e *mariadbEngine) RecordServingParameters(ctx context.Context) error {
	e.paramMu.Lock()
	defer e.paramMu.Unlock()
	return e.recordServingParametersLocked(ctx)
}

func (e *mariadbEngine) recordServingParametersLocked(ctx context.Context) error {
	return recordLastGood(ctx, e.params, e.pendingRestartParameters)
}

// Puts the last set the engine accepted back in place, for a start that failed
// after a parameter change.
func (e *mariadbEngine) RestoreLastKnownGoodParameters(ctx context.Context) (bool, error) {
	e.paramMu.Lock()
	defer e.paramMu.Unlock()
	return restoreLastGood(ctx, e.params, e.probe)
}

// The same settings under the names the catalog and the API know them by, since
// a file is written in the engine's startup spellings and those are not always
// the customer's.
func (e *mariadbEngine) catalogKeyed(values map[string]string) map[string]string {
	out := make(map[string]string, len(values))
	for name, value := range values {
		out[e.rules.catalogName(name)] = value
	}
	return out
}

// The subset of MariaDB's option-file syntax the generated drop-ins are written
// in: a group header, comments, and one `name = value` per line. Nothing else
// reaches these two files, because only rds-init and this agent write them.
func readOptionFile(path string) (map[string]string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	values := make(map[string]string)
	for line := range strings.SplitSeq(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, ";") || strings.HasPrefix(line, "[") {
			continue
		}
		// A bare option such as skip-log-bin carries no value; it is still a setting
		// whose presence or absence differs.
		name, value, _ := strings.Cut(line, "=")
		values[normaliseOptionName(name)] = strings.Trim(strings.TrimSpace(value), `"'`)
	}
	return values, nil
}

// MariaDB reads - and _ as the same character in an option name, so the two
// spellings of one setting must not compare as two settings.
func normaliseOptionName(name string) string {
	return strings.ReplaceAll(strings.TrimSpace(name), "-", "_")
}

// How long any one BACKUP STAGE waits for the metadata locks it needs. Well
// under the control plane's own quiesce timeout, so a stage that cannot take its
// lock fails and is reported rather than being abandoned mid-wait with the
// request still queued in front of live traffic.
const mariadbQuiesceLockWait = 20 * time.Second

// MariaDB's own backup API rather than FLUSH TABLES WITH READ LOCK. FTWRL turns
// a snapshot into a write outage: the whole database is read-only until it is
// released, so a control plane that died after issuing quiesce would leave the
// customer read-only for the full hold, on a schedule and unattended. Its
// acquisition phase is unbounded too, with writes queueing behind a pending lock
// request. BACKUP STAGE blocks commits rather than all writes and its flush
// stage covers Aria and MyISAM, so it yields a better consistency point while
// being far less disruptive — and it is what mariadb-backup itself uses.
func (e *mariadbEngine) Quiesce(ctx context.Context, label string, hold time.Duration) error {
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
		Name:              e.client,
		Args:              e.clientArgs(),
		Env:               []string{"PATH=" + defaultGuestPath},
		SentinelStatement: "SELECT '" + sessionSentinel + "';\n",
	})
	if err != nil {
		return fmt.Errorf("open a backup session: %w", err)
	}

	// The stages run in order and on one connection: MariaDB releases the whole
	// hold with the session that took it, which is what bounds a control plane
	// that dies mid-snapshot. Fed one at a time so a stage that cannot take its
	// locks is named rather than reported as a line number.
	stages := []string{
		fmt.Sprintf("SET SESSION lock_wait_timeout = %d;\n", int(mariadbQuiesceLockWait.Seconds())),
		"BACKUP STAGE START;\n",
		"BACKUP STAGE FLUSH;\n",
		"BACKUP STAGE BLOCK_DDL;\n",
		"BACKUP STAGE BLOCK_COMMIT;\n",
	}
	for _, sql := range stages {
		if err := session.Exec(ctx, sql); err != nil {
			if closeErr := session.Close(); closeErr != nil {
				slog.Warn("rds-agent: closing a failed backup session", "err", closeErr)
			}
			return fmt.Errorf("put the engine into backup mode at %q: %w", strings.TrimSpace(sql), err)
		}
	}

	e.beginHoldLocked(label, session, hold)
	return nil
}

// Ends the backup cleanly. A missing hold is an error rather than a silent
// success: it means the deadline fired first, so the snapshot the control plane
// just took was not taken against a held stage.
func (e *mariadbEngine) Unquiesce(ctx context.Context) error {
	held := e.takeHold()
	if held == nil {
		return errors.New("the engine is not quiesced; the backup hold had already expired")
	}

	// The release has to run on the session that took the stages, and the session
	// is closed either way — the engine ends an unreleased backup with it.
	execErr := held.session.Exec(ctx, "BACKUP STAGE END;\n")
	closeErr := held.session.Close()
	if execErr != nil {
		return fmt.Errorf("take the engine out of backup mode: %w", errors.Join(execErr, closeErr))
	}
	if closeErr != nil {
		return fmt.Errorf("close the backup session: %w", closeErr)
	}
	slog.Info("rds-agent: engine released from backup mode", "label", held.label)
	return nil
}

// Feeds sql on stdin rather than as an argument, so a statement is never visible
// in the process table.
func (e *mariadbEngine) clientRun(ctx context.Context, sql string) (string, error) {
	return e.run(ctx, command{
		Name:  e.client,
		Args:  e.clientArgs(),
		Env:   []string{"PATH=" + defaultGuestPath},
		Stdin: sql,
	})
}

// --no-defaults so no option file can move the connection this agent makes, and
// --unbuffered so a held session's output reaches the reader as each statement
// finishes rather than when a pipe buffer fills. No User: the agent is root, and
// root is what the datadir's unix_socket plugin authenticates.
func (e *mariadbEngine) clientArgs() []string {
	return []string{
		"--no-defaults", "--batch", "--skip-column-names", "--unbuffered",
		"--protocol=socket", "--socket=" + e.socket, "--user=" + mariadbSuperuser,
	}
}
