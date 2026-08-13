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
	"syscall"
	"time"

	handlers_rds "github.com/mulgadc/spinifex/spinifex/handlers/rds"
)

// The in-guest half of the engine seam. Everything engine-shaped that a
// control-plane command needs lives behind this, so the command registry stays
// engine-agnostic.
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

// Records the parameter file only after the probe observes the engine serving
// it. This is separate from apply, where static values are installed but not
// adopted until a later restart.
type servingParameterRecorder interface {
	RecordServingParameters(ctx context.Context) error
}

// Everything one engine implementation owns. The narrow interfaces above are
// what the heartbeat, the commander and the rollback guard each hold; this is
// only what the agent has to construct.
type engine interface {
	engineOps
	parameterRecovery
	servingParameterRecorder
}

// The guest layout an engine preset lays down, keyed by the engine the image
// bakes. Every default that belongs to one engine lives here, so nothing
// outside an implementation names its paths, service, port or probe.
type engineLayout struct {
	binDir    string
	dataDir   string
	socketDir string
	osUser    string
	service   string
	dataMount string
	port      int
	// Where the engine records the pid the probe checks for liveness. Empty for
	// an engine whose probe answers without one.
	pidFile string
	// Where the engine records why it would not start. Empty for an engine whose
	// image does not direct one, which leaves the probe reporting its own reason
	// alone rather than guessing at a path.
	errorLog string
	// The engine's own liveness signal, which is the only part of engineProbe
	// that is not shared.
	newProbe func(cfg config, run probeRunner) *engineProbe
	// The implementation every control-plane directive is served by. It fails
	// when the control plane this agent was built against does not offer the
	// engine the image bakes, rather than serving one it has no definition for.
	newEngine func(cfg config, run commandRunner, startSess sessionRunner, probe *engineProbe) (engine, error)
}

// The names the control plane knows these implementations by, which are also
// what setup.sh stamps into each image.
const (
	enginePostgres = "postgres"
	engineMariaDB  = "mariadb"
)

var engineLayouts = map[string]engineLayout{
	enginePostgres: {
		binDir:    "/usr/libexec/postgresql18",
		dataDir:   "/var/lib/postgresql/18/data",
		socketDir: "/run/postgresql",
		osUser:    "postgres",
		service:   "postgresql",
		dataMount: "/var/lib/postgresql",
		port:      5432,
		newProbe:  newPostgresProbe,
		newEngine: func(cfg config, run commandRunner, startSess sessionRunner, probe *engineProbe) (engine, error) {
			return newPostgresEngine(cfg, run, startSess, probe), nil
		},
	},
	engineMariaDB: {
		binDir:    "/usr/bin",
		dataDir:   "/var/lib/mysql/data",
		socketDir: "/run/mysqld",
		osUser:    "mysql",
		service:   "mariadb",
		dataMount: "/var/lib/mysql",
		port:      3306,
		// Alpine's service passes --pid-file=/run/mysqld/$RC_SVCNAME.pid, and the
		// command line beats the option file, so this is the path rather than
		// anything the drop-in names. setup.sh asserts it at build time.
		pidFile: "/run/mysqld/mariadb.pid",
		// log_error in the platform drop-in rds-init writes. Without it
		// mysqld_safe would send the server's errors to syslog, which nothing
		// collects off a guest that has no SSH.
		errorLog:  "/var/log/mysql/error.log",
		newProbe:  newMariaDBProbe,
		newEngine: newMariaDBEngineFromCatalog,
	},
}

// Builds the health probe the image's engine supplies.
func newProbe(cfg config, run probeRunner) (*engineProbe, error) {
	layout, err := layoutFor(cfg)
	if err != nil {
		return nil, err
	}
	return layout.newProbe(cfg, run), nil
}

// Builds the implementation the image bakes. Nothing here consults the control
// plane for which engine to be: the agent runs the engine it is made of, and the
// delivered engine is only ever checked against it.
func newEngine(cfg config, run commandRunner, startSess sessionRunner, probe *engineProbe) (engine, error) {
	layout, err := layoutFor(cfg)
	if err != nil {
		return nil, err
	}
	return layout.newEngine(cfg, run, startSess, probe)
}

func layoutFor(cfg config) (engineLayout, error) {
	if cfg.BakedEngine == "" {
		return engineLayout{}, fmt.Errorf("this image carries no engine stamp at %s", cfg.EngineFile)
	}
	layout, ok := engineLayouts[cfg.BakedEngine]
	if !ok {
		return engineLayout{}, fmt.Errorf("this image bakes engine %q, which this agent does not implement", cfg.BakedEngine)
	}
	return layout, nil
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
	// The statement that makes this client print the session sentinel. Read only
	// when the command is started as a held session, whose reader has to know
	// when the engine has finished each script it was fed.
	SentinelStatement string
}

// Returns stdout. stderr is folded into the error, because both clients report
// the actual SQL failure there while exiting with a bare status.
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

// Through the service manager rather than the engine's own control tool, so the
// supervisor records the state and does not restart the engine underneath a VM
// that is going down.
func serviceAction(ctx context.Context, run commandRunner, rcService, service, action string) error {
	if _, err := run(ctx, command{
		Name: rcService,
		Args: []string{service, action},
		Env:  []string{"PATH=" + defaultGuestPath},
	}); err != nil {
		return fmt.Errorf("%s the %s service: %w", action, service, err)
	}
	return nil
}

// Whether the installed set requires TLS of client connections. Read back from
// the file rather than taken from the set being applied, because the restore and
// repair paths put a file in place without holding one. Both engines' generated
// parameter files are written in the same syntax, so one reader serves them.
func installedTLSEnforcement(name, installedPath string) (bool, error) {
	values, err := readOptionFile(installedPath)
	if err != nil && !os.IsNotExist(err) {
		return false, fmt.Errorf("read the installed parameters: %w", err)
	}
	value, ok := values[name]
	if !ok {
		// The ordinary state of an instance whose resolved set predates the
		// parameter, and the whole of the migration story: it begins enforcing at
		// its next boot with no control-plane work at all.
		return true, nil
	}
	switch value {
	case "1":
		return true, nil
	case "0":
		return false, nil
	}
	// The resolver canonicalises every boolean, so anything else means the file
	// was written by something other than the platform. The permissive reading is
	// the one that must not be chosen for a setting that turns TLS off.
	return false, fmt.Errorf("the installed parameters set %s to %q, which is neither 1 nor 0", name, value)
}

// A minimal environment for a child that is not carrying secrets.
const defaultGuestPath = "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"

func redact(s, secret string) string {
	if secret == "" {
		return s
	}
	return strings.ReplaceAll(s, secret, "[REDACTED]")
}
