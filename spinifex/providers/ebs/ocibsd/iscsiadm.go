package ocibsd

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// ReplacementTimeoutSeconds is the node.session.timeo.replacement_timeout set
// on every node record: a cut session fails its queued writes after this long
// instead of holding them for the image's default.
const ReplacementTimeoutSeconds = 120

// iscsiadm exit codes (open-iscsi iscsi_err.h) that mean the work is done.
const (
	iscsiErrSessExists  = 15
	iscsiErrNoObjsFound = 21
)

const (
	defaultDeviceTimeout = 30 * time.Second
	defaultDevicePoll    = 100 * time.Millisecond
)

// CommandRunner runs a host command with no shell between, returning its
// combined output. A failed command's error must expose ExitCode().
type CommandRunner interface {
	Run(ctx context.Context, name string, args ...string) ([]byte, error)
}

// ExecRunner is the CommandRunner over os/exec.
type ExecRunner struct{}

var _ CommandRunner = ExecRunner{}

func (ExecRunner) Run(ctx context.Context, name string, args ...string) ([]byte, error) {
	out, err := exec.CommandContext(ctx, name, args...).CombinedOutput()
	if err != nil {
		return out, fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return out, nil
}

// ISCSIAdm is the Initiator over open-iscsi's iscsiadm.
type ISCSIAdm struct {
	Runner CommandRunner
	// Binary defaults to "iscsiadm"; Stat to os.Stat. Zero durations take
	// the defaults above.
	Binary        string
	Stat          func(path string) (os.FileInfo, error)
	DeviceTimeout time.Duration
	DevicePoll    time.Duration
}

var _ Initiator = (*ISCSIAdm)(nil)

// NewISCSIAdm returns an ISCSIAdm that runs iscsiadm through runner.
func NewISCSIAdm(runner CommandRunner) *ISCSIAdm {
	return &ISCSIAdm{Runner: runner}
}

func (a *ISCSIAdm) binary() string {
	if a.Binary == "" {
		return "iscsiadm"
	}
	return a.Binary
}

func portal(t Target) string { return t.IPv4 + ":" + strconv.Itoa(t.Port) }

func (a *ISCSIAdm) node(ctx context.Context, t Target, args ...string) error {
	full := append([]string{"-m", "node", "-T", t.IQN, "-p", portal(t)}, args...)
	_, err := a.Runner.Run(ctx, a.binary(), full...)
	return err
}

// Login writes the node record, with manual startup so a reboot does not log
// back in behind the daemon and the short replacement timeout, then logs in.
// No login is attempted unless both settings were written.
func (a *ISCSIAdm) Login(ctx context.Context, t Target) (string, error) {
	if err := a.node(ctx, t, "-o", "new"); err != nil {
		return "", fmt.Errorf("create node record: %w", err)
	}
	settings := []struct{ name, value string }{
		{"node.startup", "manual"},
		{"node.session.timeo.replacement_timeout", strconv.Itoa(ReplacementTimeoutSeconds)},
	}
	for _, s := range settings {
		if err := a.node(ctx, t, "-o", "update", "-n", s.name, "-v", s.value); err != nil {
			return "", fmt.Errorf("set %s: %w", s.name, err)
		}
	}
	if err := a.node(ctx, t, "-l"); err != nil && exitCode(err) != iscsiErrSessExists {
		return "", fmt.Errorf("log in: %w", err)
	}
	return a.awaitDevice(ctx, ByPathDevice(t))
}

// Logout closes any session to t and deletes its node record.
func (a *ISCSIAdm) Logout(ctx context.Context, t Target) error {
	if err := a.node(ctx, t, "-u"); err != nil && exitCode(err) != iscsiErrNoObjsFound {
		return fmt.Errorf("log out: %w", err)
	}
	if err := a.node(ctx, t, "-o", "delete"); err != nil && exitCode(err) != iscsiErrNoObjsFound {
		return fmt.Errorf("delete node record: %w", err)
	}
	return nil
}

// Sessions lists this node's sessions. A line it cannot read is skipped, so
// a session it cannot identify is never acted on.
func (a *ISCSIAdm) Sessions(ctx context.Context) ([]Target, error) {
	out, err := a.Runner.Run(ctx, a.binary(), "-m", "session")
	if err != nil {
		if exitCode(err) == iscsiErrNoObjsFound {
			return nil, nil
		}
		return nil, fmt.Errorf("list sessions: %w", err)
	}
	var sessions []Target
	for line := range strings.SplitSeq(strings.TrimSpace(string(out)), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		t, ok := parseSession(line)
		if !ok {
			slog.WarnContext(ctx, "ocibsd: skipping an iscsiadm session line it cannot read", "line", line)
			continue
		}
		sessions = append(sessions, t)
	}
	return sessions, nil
}

// parseSession reads one `iscsiadm -m session` line, such as
// "tcp: [1] 169.254.2.2:3260,1 iqn.2015-12.com.oracleiaas:abc (non-flash)".
func parseSession(line string) (Target, bool) {
	fields := strings.Fields(line)
	if len(fields) < 4 || fields[0] != "tcp:" {
		return Target{}, false
	}
	addr, _, _ := strings.Cut(fields[2], ",")
	i := strings.LastIndexByte(addr, ':')
	if i <= 0 {
		return Target{}, false
	}
	port, err := strconv.Atoi(addr[i+1:])
	if err != nil || port <= 0 {
		return Target{}, false
	}
	return Target{IQN: fields[3], IPv4: addr[:i], Port: port}, true
}

func (a *ISCSIAdm) awaitDevice(ctx context.Context, path string) (string, error) {
	stat, timeout, interval := a.Stat, a.DeviceTimeout, a.DevicePoll
	if stat == nil {
		stat = os.Stat
	}
	if timeout <= 0 {
		timeout = defaultDeviceTimeout
	}
	if interval <= 0 {
		interval = defaultDevicePoll
	}
	err := poll(ctx, timeout, interval, "device "+path+" appearing", func(context.Context) (bool, error) {
		_, err := stat(path)
		if err == nil {
			return true, nil
		}
		if !errors.Is(err, os.ErrNotExist) {
			return false, fmt.Errorf("stat %s: %w", path, err)
		}
		return false, nil
	})
	if errors.Is(err, errGaveUp) {
		return "", fmt.Errorf("device %s did not appear", path)
	}
	if err != nil {
		return "", err
	}
	return path, nil
}

// exitCode is the exit status err carries, or -1 when it carries none.
func exitCode(err error) int {
	var coded interface{ ExitCode() int }
	if errors.As(err, &coded) {
		return coded.ExitCode()
	}
	return -1
}
