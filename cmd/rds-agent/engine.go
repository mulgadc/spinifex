package main

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"

	handlers_rds "github.com/mulgadc/spinifex/spinifex/handlers/rds"
)

// The in-guest half of the engine seam. Everything PostgreSQL-shaped that a
// control-plane command needs lives behind this, so the command registry above
// stays engine-agnostic.
type engineOps interface {
	// Rotates the master role's password live. Never persisted anywhere in the
	// guest: it exists only for the length of this call (D8).
	SetPassword(ctx context.Context, username, password string) error
	// Installs the resolved parameter set and reloads. Returns the settings the
	// engine accepted but will not honour until it restarts.
	ApplyParameters(ctx context.Context, params []handlers_rds.Parameter) ([]string, error)
	// Shuts the engine down cleanly, so the data volume is checkpointed before
	// the VM is stopped or snapshotted.
	Stop(ctx context.Context) error
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
	psql      string
	rcService string
	service   string
	pgData    string
	socketDir string
	osUser    string
	// Set from the bootstrap config, on a different goroutine than the commands
	// that read it.
	port atomic.Int64
}

var _ engineOps = (*postgresEngine)(nil)

func newPostgresEngine(cfg config, run commandRunner) *postgresEngine {
	e := &postgresEngine{
		run:       run,
		psql:      filepath.Join(cfg.PGBin, "psql"),
		rcService: cfg.RCService,
		service:   cfg.EngineService,
		pgData:    cfg.PGData,
		socketDir: cfg.SocketDir,
		osUser:    cfg.PGUser,
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
	const sql = `\getenv master RDS_MASTER_USERNAME
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

// The parameters go beside the data rather than into /etc, matching rds-init: a
// class change boots a fresh root volume, which would otherwise revert them.
func (e *postgresEngine) ApplyParameters(ctx context.Context, params []handlers_rds.Parameter) ([]string, error) {
	if err := e.installParameters(params); err != nil {
		return nil, err
	}
	if _, err := e.psqlRun(ctx, "SELECT pg_reload_conf();\n"); err != nil {
		return nil, fmt.Errorf("reload the engine configuration: %w", err)
	}

	// Read after the reload: a setting only becomes pending_restart once the
	// engine has seen the new value and declined to adopt it live.
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

// Written as root and handed to the engine's user, at the same path and mode
// rds-init installs it at, so a later boot overwrites rather than shadows it.
func (e *postgresEngine) installParameters(params []handlers_rds.Parameter) error {
	dir := filepath.Join(e.pgData, "conf.d")
	path := filepath.Join(dir, "10-rds-parameters.conf")
	tmp := path + ".new"

	if err := os.WriteFile(tmp, []byte(renderParameters(params)), 0o600); err != nil {
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
	if _, err := e.run(ctx, command{
		Name: e.rcService,
		Args: []string{e.service, "stop"},
		Env:  []string{"PATH=" + defaultGuestPath},
	}); err != nil {
		return fmt.Errorf("stop the %s service: %w", e.service, err)
	}
	return nil
}

// A minimal environment for a child that is not carrying secrets.
const defaultGuestPath = "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"

// Feeds sql on stdin rather than as an argument, so a statement is never
// visible in the process table.
func (e *postgresEngine) psqlRun(ctx context.Context, sql string, env ...string) (string, error) {
	return e.run(ctx, command{
		Name: e.psql,
		Args: []string{
			"--no-psqlrc", "--quiet", "--no-align", "--tuples-only",
			"-v", "ON_ERROR_STOP=1",
			"-h", e.socketDir,
			"-p", strconv.FormatInt(e.port.Load(), 10),
			"-U", e.osUser,
			"-d", "postgres",
			"-f", "-",
		},
		Env:   append([]string{"PATH=" + defaultGuestPath}, env...),
		Stdin: sql,
		User:  e.osUser,
	})
}

func redact(s, secret string) string {
	if secret == "" {
		return s
	}
	return strings.ReplaceAll(s, secret, "[REDACTED]")
}
