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
	// The catalog's data type for a setting, which decides how its value is
	// rendered into SQL. Empty for a name the catalog does not carry.
	dataType func(name string) string
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
		dataType: func(name string) string {
			spec, ok := meta.LookupParameter(name)
			if !ok {
				return ""
			}
			return spec.DataType
		},
	}
}

// Reached over its unix socket as root, which the datadir's unix_socket plugin
// authenticates from the connecting process' own uid. That is why the agent
// holds no password of its own.
type mariadbEngine struct {
	quiesceState
	parameterManager

	rules     controlPlaneRules
	run       commandRunner
	startSess sessionRunner
	client    string
	socket    string
	rcService string
	service   string
}

var _ engine = (*mariadbEngine)(nil)

const (
	mariadbClientBinary = "mariadb"
	mariadbAdminBinary  = "mariadb-admin"
	// The account mariadb-install-db creates for the unix_socket plugin, which
	// rds-init keeps for the platform and never hands to the customer.
	mariadbSuperuser                  = "root"
	mariadbProbeConnectTimeoutSeconds = 3
	// How much of the engine's error log a probe reason carries. The server names
	// what it refused on in its last few lines, and the reason reaches a customer
	// through StatusInfos, so this quotes the tail rather than the whole file.
	mariadbErrorLogTailBytes = 4096
	mariadbErrorLogTailLines = 4
	// How much of the probe client's stderr a reason carries, on the same grounds
	// as the log tail above.
	mariadbProbeStderrMaxBytes = 512
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
		socket:    mariadbSocketPath(cfg),
		rcService: cfg.RCService,
		service:   cfg.EngineService,
		parameterManager: parameterManager{
			probe: probe,
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
		},
	}
}

func mariadbSocketPath(cfg config) string {
	return filepath.Join(cfg.SocketDir, mariadbSocketFile)
}

// Every local connection, from the probe's ping to a held quiesce session.
// --no-defaults so no option file can move it; --skip-ssl because the serving
// cert names the endpoint rather than this socket, so a verifying client fails.
func mariadbSocketConnectArgs(socket string) []string {
	return []string{
		"--no-defaults", "--protocol=socket", "--socket=" + socket,
		"--user=" + mariadbSuperuser, "--skip-ssl",
	}
}

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

// Three stages: during InnoDB crash recovery mariadbd opens neither socket nor
// port, so a ping cannot tell a recovering engine from an absent one — and
// reading recovery as absent has the rollback guard restart a server mid-replay.
func mariadbProbeState(cfg config, run probeRunner, alive processLivenessFn) probeStateFn {
	pidFile, socket := cfg.EnginePidFile, mariadbSocketPath(cfg)
	admin, client := filepath.Join(cfg.EngineBinDir, mariadbAdminBinary), filepath.Join(cfg.EngineBinDir, mariadbClientBinary)
	// The probe alone bounds the connect: it must come back and report, where a
	// statement the agent issues is allowed to take as long as the engine needs.
	connect := append(mariadbSocketConnectArgs(socket),
		fmt.Sprintf("--connect-timeout=%d", mariadbProbeConnectTimeoutSeconds))
	probeTimeout := cfg.EngineProbeTimeout
	if probeTimeout <= 0 {
		probeTimeout = defaultEngineProbeTimeout
	}
	runBounded := func(ctx context.Context, name string, args ...string) (int, string, error) {
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
		// not serving yet rather than one that is gone. Reporting absent against a
		// live server would have the rollback guard restart one making progress.
		switch code, stderr, err := runBounded(ctx, admin, append(slices.Clone(connect), "ping")...); {
		case err != nil:
			return engineRecovering, fmt.Sprintf("engine probe could not run: %v", err)
		case code != 0:
			// A server that is genuinely recovering and one that refused the client
			// are indistinguishable from the socket. The server states its side in
			// its own log, and the client states its side on stderr.
			return engineRecovering, withProbeStderr(withEngineLogTail(
				"engine is not answering on its socket yet (startup or crash recovery)",
				cfg.EngineErrorLog), stderr)
		}

		// ping answers even on ER_ACCESS_DENIED, so on its own it certifies a server
		// that may be unable to execute anything. The statement rides argv rather
		// than stdin: it carries nothing secret and a probe reads no result back.
		query := append(slices.Clone(connect), "--batch", "--skip-column-names", "--execute=SELECT 1")
		switch code, stderr, err := runBounded(ctx, client, query...); {
		case err != nil:
			return engineRecovering, fmt.Sprintf("engine probe could not run: %v", err)
		case code != 0:
			return engineRecovering, withProbeStderr(
				"engine answered its socket but would not execute a statement", stderr)
		}
		return engineServing, ""
	}
}

// Appends what the probe client said on stderr. A client that fails during the
// handshake is refused by the server without the server ever logging why, so
// this is the only account of that half of the exchange.
func withProbeStderr(reason, stderr string) string {
	stderr = strings.TrimSpace(stderr)
	if stderr == "" {
		return reason
	}
	return reason + "; the probe client reported: " + collapseProbeStderr(stderr)
}

// One line, bounded: the reason reaches a customer through StatusInfos, and a
// client that repeats itself per attempt would otherwise crowd out the rest.
func collapseProbeStderr(stderr string) string {
	line := strings.Join(strings.Fields(strings.ReplaceAll(stderr, "\n", " ")), " ")
	if len(line) > mariadbProbeStderrMaxBytes {
		return line[:mariadbProbeStderrMaxBytes] + "..."
	}
	return line
}

// Appends the last few lines of the engine's error log to reason. An unset path,
// an unreadable file or an empty one leaves reason alone: the probe's own
// statement is still true, and a guessed cause would be worse than none.
func withEngineLogTail(reason, path string) string {
	tail := engineLogTail(path)
	if tail == "" {
		return reason
	}
	return reason + "; the engine's log ends: " + tail
}

func engineLogTail(path string) string {
	if path == "" {
		return ""
	}
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer func() { _ = f.Close() }()

	info, err := f.Stat()
	if err != nil {
		return ""
	}
	offset := max(info.Size()-mariadbErrorLogTailBytes, 0)
	buf := make([]byte, info.Size()-offset)
	if _, err := f.ReadAt(buf, offset); err != nil {
		return ""
	}

	lines := strings.Split(string(buf), "\n")
	// A read that started mid-file almost certainly started mid-line, and half a
	// line is more misleading than one fewer.
	if offset > 0 && len(lines) > 0 {
		lines = lines[1:]
	}
	kept := make([]string, 0, mariadbErrorLogTailLines)
	for i := len(lines) - 1; i >= 0 && len(kept) < mariadbErrorLogTailLines; i-- {
		if line := strings.TrimSpace(lines[i]); line != "" {
			kept = append(kept, line)
		}
	}
	slices.Reverse(kept)
	return strings.Join(kept, " | ")
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
// control plane wants goes through RebootDBInstance, which cycles the VM. The
// serving copy is refreshed first, since the server starts on what is installed.
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

// The spellings MariaDB parses for a boolean system variable, and the only ones
// the catalog's own enum narrows a boolean to.
var mariadbBooleanValues = map[string]string{
	"on": "ON", "off": "OFF", "true": "ON", "false": "OFF", "1": "ON", "0": "OFF",
}

// One SET GLOBAL right-hand side. MariaDB refuses a quoted literal for a numeric
// or boolean with ER_WRONG_TYPE_FOR_VAR, so the catalog's data type picks the
// form; re-rendering from the parsed value is what keeps it safe to interpolate.
func mariadbSetValue(dataType, value string) (string, error) {
	switch dataType {
	case handlers_rds.ParamTypeInteger:
		n, err := strconv.ParseInt(strings.TrimSpace(value), 10, 64)
		if err != nil {
			return "", fmt.Errorf("value %q is not an integer", value)
		}
		return strconv.FormatInt(n, 10), nil
	case handlers_rds.ParamTypeReal:
		f, err := strconv.ParseFloat(strings.TrimSpace(value), 64)
		if err != nil {
			return "", fmt.Errorf("value %q is not a real number", value)
		}
		return strconv.FormatFloat(f, 'g', -1, 64), nil
	case handlers_rds.ParamTypeBoolean:
		if literal, ok := mariadbBooleanValues[strings.ToLower(strings.TrimSpace(value))]; ok {
			return literal, nil
		}
		return "", fmt.Errorf("value %q is not a boolean", value)
	default:
		return "'" + sqlLiteral(value) + "'", nil
	}
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
	// This runs as the server's own superuser, so a reserved name in the payload
	// would rotate the platform's account. The whole rule matters here, not just
	// the reserved set: the name is interpolated below rather than client-quoted.
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

// Installs the resolved set and applies the half a running server can adopt.
// MariaDB re-reads no file while up, so SET GLOBAL is the only immediate path.
// It has no offline parser, so the safety net is the boot-time rollback.
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

// The dynamic half, in one invocation that stops at the first refusal so the
// server names the setting it would not take. Only catalog names are emitted,
// which is what makes the identifier safe to build into the statement.
func (e *mariadbEngine) applyDynamicParameters(ctx context.Context, params []handlers_rds.Parameter) error {
	var b strings.Builder
	b.WriteString("SET SESSION sql_mode = 'NO_ENGINE_SUBSTITUTION';\n")
	applied := 0
	for _, p := range params {
		if p.Name == "" || e.rules.isStatic(p.Name) {
			continue
		}
		value, err := mariadbSetValue(e.rules.dataType(p.Name), p.Value)
		if err != nil {
			return fmt.Errorf("apply the dynamic parameters: %s: %w", p.Name, err)
		}
		fmt.Fprintf(&b, "SET GLOBAL %s = %s;\n", p.Name, value)
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

// MariaDB has no pending_restart, so this is computed from the files: the
// catalog-static keys whose installed value differs from the one the server
// started on. Live values are not compared — mysqld silently rewrites them.
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

// How long any one BACKUP STAGE waits for its metadata locks. Well under the
// control plane's quiesce timeout, so a stage that cannot take its lock is
// reported rather than left queued in front of live traffic.
const mariadbQuiesceLockWait = 20 * time.Second

// MariaDB's own backup API rather than FLUSH TABLES WITH READ LOCK: FTWRL makes
// the whole database read-only for the full hold, and its acquisition phase is
// unbounded. BACKUP STAGE blocks commits instead, and covers Aria and MyISAM.
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

	// In order and on one connection: MariaDB releases the hold with the session
	// that took it, which bounds a control plane that dies mid-snapshot. Fed one
	// at a time so a stage that cannot take its locks is named.
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

func (e *mariadbEngine) Unquiesce(ctx context.Context) error {
	return e.releaseHold(ctx, "BACKUP STAGE END;\n")
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

// --unbuffered so a held session's output reaches the reader as each statement
// finishes rather than when a pipe buffer fills. No User on the command: the
// connection args carry it, and the agent runs as the account they name.
func (e *mariadbEngine) clientArgs() []string {
	return append(mariadbSocketConnectArgs(e.socket),
		"--batch", "--skip-column-names", "--unbuffered")
}
