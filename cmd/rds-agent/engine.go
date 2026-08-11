package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	handlers_rds "github.com/mulgadc/spinifex/spinifex/handlers/rds"
)

// The in-guest half of the engine seam. Everything PostgreSQL-shaped that a
// control-plane command needs lives behind this, so the command registry above
// stays engine-agnostic.
type engineOps interface {
	// Rotates the master role's password live. Never persisted anywhere in the
	// guest: it exists only for the length of this call.
	SetPassword(ctx context.Context, username, password string) error
	// Installs the resolved parameter set and reloads. Returns the settings the
	// engine accepted but will not honour until it restarts.
	ApplyParameters(ctx context.Context, params []handlers_rds.Parameter) ([]string, error)
	// Shuts the engine down cleanly, so the data volume is checkpointed before
	// the VM is stopped or snapshotted.
	Stop(ctx context.Context) error
	// Puts the engine into backup mode so the datadir can be snapshotted at a
	// checkpoint. Releases itself after hold, whatever happens to the caller.
	Quiesce(ctx context.Context, label string, hold time.Duration) error
	// Takes the engine back out of backup mode. Fails when no hold is active,
	// which is how a hold that expired mid-snapshot becomes visible.
	Unquiesce(ctx context.Context) error
}

// The rollback half of the parameter path, kept off engineOps because it is not
// a control-plane directive: nothing issues it, the agent runs it when the
// engine fails to come up after a parameter change.
type parameterRecovery interface {
	// Reports whether the installed set differed and was replaced.
	RestoreLastKnownGoodParameters(ctx context.Context) (bool, error)
	Restart(ctx context.Context) error
}

// Records the parameter file only after the probe observes the postmaster
// serving it. This is separate from apply, where static values are installed
// but not adopted until a later restart.
type servingParameterRecorder interface {
	RecordServingParameters(ctx context.Context) error
}

// One child process. Env replaces the agent's own environment rather than
// extending it, so a secret placed here reaches only the process that needs it.
type command struct {
	Name  string
	Args  []string
	Env   []string
	Stdin string
	// The OS user to drop to before exec. Empty runs as the agent itself.
	User string
}

// Returns stdout. stderr is folded into the error, because psql reports the
// actual SQL failure there while exiting with a bare status.
type commandRunner func(ctx context.Context, c command) (string, error)

func execCommandRunner(ctx context.Context, c command) (string, error) {
	cmd := exec.CommandContext(ctx, c.Name, c.Args...)
	cmd.Env = c.Env
	if c.Stdin != "" {
		cmd.Stdin = strings.NewReader(c.Stdin)
	}
	if c.User != "" {
		credential, err := lookupCredential(c.User)
		if err != nil {
			return "", err
		}
		cmd.SysProcAttr = &syscall.SysProcAttr{Credential: credential}
	}

	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		message := strings.TrimSpace(stderr.String())
		if message == "" {
			message = err.Error()
		}
		return stdout.String(), fmt.Errorf("%s: %s", filepath.Base(c.Name), message)
	}
	return stdout.String(), nil
}

func lookupCredential(username string) (*syscall.Credential, error) {
	u, err := user.Lookup(username)
	if err != nil {
		return nil, fmt.Errorf("resolve user %s: %w", username, err)
	}
	uid, err := strconv.ParseUint(u.Uid, 10, 32)
	if err != nil {
		return nil, fmt.Errorf("parse uid of %s: %w", username, err)
	}
	gid, err := strconv.ParseUint(u.Gid, 10, 32)
	if err != nil {
		return nil, fmt.Errorf("parse gid of %s: %w", username, err)
	}
	return &syscall.Credential{Uid: uint32(uid), Gid: uint32(gid)}, nil
}

// The engine is reached over its unix socket under peer authentication, so the
// agent drops to the postgres OS user rather than holding a password of its own.
type postgresEngine struct {
	run       commandRunner
	startSess sessionRunner
	psql      string
	rcService string
	service   string
	pgData    string
	socketDir string
	osUser    string
	probe     *engineProbe
	// Set from the bootstrap config, on a different goroutine than the commands
	// that read it.
	port atomic.Int64

	// Serializes parameter installs, serving snapshots and rollback restores so
	// none can copy or replace an intermediate configuration.
	paramMu       sync.Mutex
	repairTimeout time.Duration
	repairPoll    time.Duration

	// The backup mode currently held, if any. Guarded because the expiry timer
	// releases it on its own goroutine.
	mu   sync.Mutex
	held *quiesceHold
}

var (
	_ engineOps                = (*postgresEngine)(nil)
	_ parameterRecovery        = (*postgresEngine)(nil)
	_ servingParameterRecorder = (*postgresEngine)(nil)
)

const (
	parameterRepairTimeout = 90 * time.Second
	parameterRepairPoll    = time.Second
)

func newPostgresEngine(cfg config, run commandRunner, startSess sessionRunner, probe *engineProbe) *postgresEngine {
	e := &postgresEngine{
		run:           run,
		startSess:     startSess,
		psql:          filepath.Join(cfg.PGBin, "psql"),
		rcService:     cfg.RCService,
		service:       cfg.EngineService,
		pgData:        cfg.PGData,
		socketDir:     cfg.SocketDir,
		osUser:        cfg.PGUser,
		probe:         probe,
		repairTimeout: parameterRepairTimeout,
		repairPoll:    parameterRepairPoll,
	}
	e.port.Store(int64(cfg.EnginePort))
	return e
}

func (e *postgresEngine) setPort(port int) {
	e.port.Store(int64(port))
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
	if err := handlers_rds.EnginePostgres().ValidateUsernameNotReserved(username); err != nil {
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

// The include the resolved parameter set is rendered to, and the copy of the
// last one the engine accepted. Both live beside the data rather than in /etc,
// matching rds-init: a class change boots a fresh root volume, which would
// otherwise revert them when a fresh root volume is used.
const (
	parametersFileName = "10-rds-parameters.conf"
	// Deliberately not a .conf name: include_dir globs *.conf, so the rollback
	// copy must not be read as a second set of settings.
	lastGoodFileName = "10-rds-parameters.last-good"
)

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
	_, restore, err := e.installParameters(params)
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
	repairCtx, cancel := context.WithTimeout(ctx, e.repairTimeout)
	defer cancel()

	if err := e.Restart(repairCtx); err != nil {
		return nil, fmt.Errorf("start the engine on the repaired parameter set: %w", err)
	}

	timer := time.NewTimer(0)
	defer timer.Stop()
	lastMessage := "engine did not respond"
	for {
		select {
		case <-repairCtx.Done():
			if ctx.Err() != nil {
				return nil, fmt.Errorf("wait for the engine on the repaired parameter set: %w", ctx.Err())
			}
			return nil, fmt.Errorf("wait for the engine on the repaired parameter set: %s: %w", lastMessage, repairCtx.Err())
		case <-timer.C:
		}

		state, message := e.probe.state(repairCtx)
		if state == engineServing {
			pending, err := e.pendingRestartParameters(repairCtx)
			if err == nil {
				return pending, nil
			}
			lastMessage = err.Error()
		} else if message != "" {
			lastMessage = message
		}
		timer.Reset(e.repairPoll)
	}
}

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

func (e *postgresEngine) parametersPath() string {
	return filepath.Join(e.pgData, "conf.d", parametersFileName)
}

func (e *postgresEngine) lastGoodPath() string {
	return filepath.Join(e.pgData, "conf.d", lastGoodFileName)
}

// Written as root and handed to the engine's user, at the same path and mode
// rds-init installs it at, so a later boot overwrites rather than shadows it.
// Returns the content that was there and a func putting it back, so the caller
// can undo the install without re-rendering anything.
func (e *postgresEngine) installParameters(params []handlers_rds.Parameter) (previous []byte, restore func() error, err error) {
	path := e.parametersPath()
	// A missing file is the first-ever apply, and its rollback is a removal.
	previous, readErr := os.ReadFile(path)
	if readErr != nil && !os.IsNotExist(readErr) {
		return nil, nil, fmt.Errorf("read the installed parameters at %s: %w", path, readErr)
	}
	existed := readErr == nil

	if err := e.writeEngineFile(path, []byte(renderParameters(params))); err != nil {
		return nil, nil, err
	}
	return previous, func() error {
		if !existed {
			if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
				return fmt.Errorf("withdraw the rejected parameters at %s: %w", path, err)
			}
			return nil
		}
		return e.writeEngineFile(path, previous)
	}, nil
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
	if _, err := os.Stat(e.parametersPath()); err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("inspect the serving parameters: %w", err)
	}
	pending, err := e.pendingRestartParameters(ctx)
	if err != nil {
		return err
	}
	if len(pending) > 0 {
		return nil
	}

	content, err := os.ReadFile(e.parametersPath())
	if err != nil {
		return fmt.Errorf("read the serving parameters: %w", err)
	}
	lastGood, err := os.ReadFile(e.lastGoodPath())
	if err == nil && bytes.Equal(content, lastGood) {
		return nil
	}
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("read the last known good parameters: %w", err)
	}
	return e.writeEngineFile(e.lastGoodPath(), content)
}

// Puts the last set the engine accepted back in place, for a restart that failed
// after a parameter change. The probe is checked again under the parameter lock
// so a repair that just brought the engine back cannot be reversed.
func (e *postgresEngine) RestoreLastKnownGoodParameters(ctx context.Context) (bool, error) {
	e.paramMu.Lock()
	defer e.paramMu.Unlock()

	if state, _ := e.probe.state(ctx); state != engineAbsent {
		return false, nil
	}
	lastGood, err := os.ReadFile(e.lastGoodPath())
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, fmt.Errorf("read the last known good parameters: %w", err)
	}
	current, err := os.ReadFile(e.parametersPath())
	if err != nil && !os.IsNotExist(err) {
		return false, fmt.Errorf("read the installed parameters: %w", err)
	}
	if bytes.Equal(current, lastGood) {
		return false, nil
	}
	if err := e.writeEngineFile(e.parametersPath(), lastGood); err != nil {
		return false, err
	}
	return true, nil
}

// Atomic: a temp file in the same directory, renamed over the target. The temp
// name deliberately does not end in .conf, so a crash between write and rename
// cannot leave the engine reading a half-written include.
func (e *postgresEngine) writeEngineFile(path string, content []byte) error {
	tmp := path + ".new"
	if err := os.WriteFile(tmp, content, 0o600); err != nil {
		return fmt.Errorf("write %s: %w", tmp, err)
	}
	if err := e.chownToEngine(tmp); err != nil {
		os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("install %s: %w", path, err)
	}
	return nil
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

// The engine reads its own config, so a root-owned file would be unreadable to
// it. A guest with no such user is a broken image, not something to work around.
func (e *postgresEngine) chownToEngine(path string) error {
	credential, err := lookupCredential(e.osUser)
	if err != nil {
		return err
	}
	if err := os.Chown(path, int(credential.Uid), int(credential.Gid)); err != nil {
		return fmt.Errorf("hand %s to %s: %w", path, e.osUser, err)
	}
	return nil
}

// Through the service manager rather than pg_ctl, so the supervisor records the
// engine as stopped and does not restart it underneath a VM that is going down.
func (e *postgresEngine) Stop(ctx context.Context) error {
	return e.serviceAction(ctx, "stop")
}

// Only the parameter rollback calls this: a restart the control plane wants goes
// through RebootDBInstance, which cycles the VM.
func (e *postgresEngine) Restart(ctx context.Context) error {
	return e.serviceAction(ctx, "restart")
}

func (e *postgresEngine) serviceAction(ctx context.Context, action string) error {
	if _, err := e.run(ctx, command{
		Name: e.rcService,
		Args: []string{e.service, action},
		Env:  []string{"PATH=" + defaultGuestPath},
	}); err != nil {
		return fmt.Errorf("%s the %s service: %w", action, e.service, err)
	}
	return nil
}

// A minimal environment for a child that is not carrying secrets.
const defaultGuestPath = "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"

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
		"-p", strconv.FormatInt(e.port.Load(), 10),
		"-U", e.osUser,
		"-d", "postgres",
		"-f", "-",
	}
}

func redact(s, secret string) string {
	if secret == "" {
		return s
	}
	return strings.ReplaceAll(s, secret, "[REDACTED]")
}
