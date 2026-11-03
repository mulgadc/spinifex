//go:build lambdatestlink

package testlink

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/mulgadc/spinifex/spinifex/domains/lambda/execution"
	"github.com/mulgadc/spinifex/spinifex/runtime/compute/lambda/attempt"
	"github.com/stretchr/testify/require"
)

func TestArmCommand_DropsInheritedLoaderSettings(t *testing.T) {
	t.Setenv("LD_PRELOAD", "/evil.so")
	t.Setenv("LD_LIBRARY_PATH", "/evil")
	t.Setenv("SPX_HARDKILL_MS", "1")
	t.Setenv("KEEP_ME", "yes")
	cmd := exec.Command("/bin/true")
	cmd.ExtraFiles = []*os.File{os.Stdin}
	hk, err := armCommand(cmd, "/lib/hardkill.so", 90*time.Second)
	require.NoError(t, err)
	defer hk.close()

	var loader, backstop []string
	for _, kv := range cmd.Env {
		switch {
		case strings.HasPrefix(kv, "LD_"):
			loader = append(loader, kv)
		case strings.HasPrefix(kv, "SPX_HARDKILL_"):
			backstop = append(backstop, kv)
		}
	}
	require.Equal(t, []string{"LD_PRELOAD=/lib/hardkill.so"}, loader)
	require.ElementsMatch(t, []string{"SPX_HARDKILL_MS=90000", "SPX_HARDKILL_ACK_FD=4",
		"SPX_HARDKILL_NONCE=" + hk.nonce}, backstop)
	require.Contains(t, cmd.Env, "KEEP_ME=yes")
	require.Len(t, cmd.ExtraFiles, 2)
	require.Len(t, hk.nonce, 32)
}

// runArmed runs script as the "VM" under attempt.Run with a backstop and
// reports whether the step after the ack ran.
func runArmed(t *testing.T, script string, wait time.Duration) (bool, attempt.Outcome, error) {
	t.Helper()
	cmd := exec.Command("/bin/sh", "-c", script)
	hk, err := armCommand(cmd, "/nonexistent/hardkill.so", time.Minute)
	require.NoError(t, err)
	defer hk.close()
	cmd.Env = append(cmd.Env, "HK_WRONG_NONCE=0123456789abcdef0123456789abcdef")
	sent := false
	out, err := attempt.Run(context.Background(), cmd, 30*time.Second, func(ctx context.Context) error {
		if err := hk.await(ctx, cmd.Process.Pid, wait); err != nil {
			return err
		}
		sent = true
		return nil
	})
	return sent, out, err
}

const ackLine = `printf 'armed %s %s %s\n' "$$" "$SPX_HARDKILL_MS" `

func TestHardKill_FrameWaitsForTheExactAck(t *testing.T) {
	sent, out, err := runArmed(t, `eval "`+ackLine+`\"\$SPX_HARDKILL_NONCE\" >&$SPX_HARDKILL_ACK_FD"; exec sleep 60`, 2*time.Second)
	require.NoError(t, err)
	require.True(t, sent)
	require.True(t, out.Stopped)
}

// Each failure kills the VM before anything is sent and proves the stop.
func TestHardKill_MissingOrWrongAckSendsNothing(t *testing.T) {
	cases := map[string]string{
		"silent":         `exec sleep 60`,
		"exits early":    `exit 0`,
		"wrong nonce":    `eval "` + ackLine + `\"\$HK_WRONG_NONCE\" >&$SPX_HARDKILL_ACK_FD"; exec sleep 60`,
		"wrong pid":      `eval "printf 'armed 1 %s %s\n' \"\$SPX_HARDKILL_MS\" \"\$SPX_HARDKILL_NONCE\" >&$SPX_HARDKILL_ACK_FD"; exec sleep 60`,
		"wrong deadline": `eval "printf 'armed %s 1 %s\n' \"\$\$\" \"\$SPX_HARDKILL_NONCE\" >&$SPX_HARDKILL_ACK_FD"; exec sleep 60`,
		"trailing data":  `eval "` + ackLine + `\"\$SPX_HARDKILL_NONCE\" extra >&$SPX_HARDKILL_ACK_FD"; exec sleep 60`,
	}
	for name, script := range cases {
		t.Run(name, func(t *testing.T) {
			start := time.Now()
			sent, out, err := runArmed(t, script, 500*time.Millisecond)
			require.False(t, sent, "no frame without a valid ack")
			require.True(t, out.Stopped, "the VM stop is proven")
			require.Error(t, err)
			if name != "exits early" {
				require.ErrorIs(t, err, ErrNotArmed)
			}
			require.Less(t, time.Since(start), 10*time.Second)
		})
	}
}

// The real initializer, preloaded into a process that ignores its parent,
// kills that process at the deadline after acknowledging the armed timer.
func buildInitializer(t *testing.T) string {
	t.Helper()
	cc, err := exec.LookPath("cc")
	if err != nil {
		t.Skip("no C compiler to build the initializer")
	}
	lib := filepath.Join(t.TempDir(), "hardkill.so")
	build := exec.Command(cc, "-O2", "-Wall", "-Wextra", "-Werror", "-shared", "-fPIC", "-o", lib, "hardkill/hardkill.c", "-lrt")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build initializer: %v: %s", err, out)
	}
	return lib
}

func requireSIGKILL(t *testing.T, err error) {
	t.Helper()
	var exit *exec.ExitError
	require.True(t, errors.As(err, &exit), "got %v", err)
	ws := exit.Sys().(syscall.WaitStatus)
	require.True(t, ws.Signaled(), "exited %d instead of being killed", ws.ExitStatus())
	require.Equal(t, syscall.SIGKILL, ws.Signal())
}

// A loaded initializer that cannot arm or acknowledge kills its process
// before main, so the target program never runs.
func TestHardKill_InitializerFailureNeverReachesMain(t *testing.T) {
	lib := buildInitializer(t)
	nonce := "SPX_HARDKILL_NONCE=0123456789abcdef0123456789abcdef"
	cases := map[string][]string{
		"no settings":     nil,
		"bad deadline":    {"SPX_HARDKILL_MS=12x", "SPX_HARDKILL_ACK_FD=3", nonce},
		"zero deadline":   {"SPX_HARDKILL_MS=0", "SPX_HARDKILL_ACK_FD=3", nonce},
		"low fd":          {"SPX_HARDKILL_MS=1000", "SPX_HARDKILL_ACK_FD=2", nonce},
		"short nonce":     {"SPX_HARDKILL_MS=1000", "SPX_HARDKILL_ACK_FD=3", "SPX_HARDKILL_NONCE=abc"},
		"ack fd not open": {"SPX_HARDKILL_MS=1000", "SPX_HARDKILL_ACK_FD=97", nonce},
	}
	for name, env := range cases {
		t.Run(name, func(t *testing.T) {
			marker := filepath.Join(t.TempDir(), "ran")
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, "/usr/bin/touch", marker)
			cmd.Cancel = func() error { return cmd.Process.Signal(syscall.SIGTERM) }
			cmd.Env = append([]string{"LD_PRELOAD=" + lib}, env...)
			requireSIGKILL(t, cmd.Run())
			require.NoFileExists(t, marker)
		})
	}
}

func TestHardKill_InitializerKillsItsOwnProcess(t *testing.T) {
	lib := buildInitializer(t)
	cmd := exec.Command("/bin/sleep", "60")
	hk, err := armCommand(cmd, lib, 300*time.Millisecond)
	require.NoError(t, err)
	defer hk.close()
	require.NoError(t, cmd.Start())
	require.NoError(t, hk.await(context.Background(), cmd.Process.Pid, 5*time.Second))

	start := time.Now()
	requireSIGKILL(t, cmd.Wait())
	require.Less(t, time.Since(start), 5*time.Second)
}

func TestDaemon_InitializerCheckRefusesBeforeTheGrantStarts(t *testing.T) {
	code := []byte("zip bytes")
	srv := codeServer(t, code)
	lib := filepath.Join(t.TempDir(), "hardkill.so")
	require.NoError(t, os.WriteFile(lib, []byte("library"), 0o600))
	sum := sha256.Sum256([]byte("library"))
	cases := map[string]func(d *Daemon){
		"not configured": func(d *Daemon) { d.Initializer, d.InitializerSHA256 = "", "" },
		"missing":        func(d *Daemon) { d.Initializer = lib + ".gone" },
		"replaced":       func(d *Daemon) { d.InitializerSHA256 = strings.Repeat("0", 64) },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			g := grant(t, code)
			ips := &fakeIPs{}
			d := g.daemon(&Daemon{IPs: ips, Taps: &fakeTaps{}, HTTP: srv.Client(), WorkDir: t.TempDir()})
			d.Initializer, d.InitializerSHA256 = lib, hex.EncodeToString(sum[:])
			mutate(d)
			rep := d.Handle(context.Background(), g.request(srv.URL, code))
			require.Equal(t, OutcomeRefused, rep.Outcome)
			require.Contains(t, rep.Error, "hard-kill initializer")
			require.Empty(t, ips.allocated)
			require.Equal(t, execution.StateGranted, g.state(t))
		})
	}
}
