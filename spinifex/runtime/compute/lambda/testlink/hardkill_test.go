//go:build lambdatestlink

package testlink

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
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
	"golang.org/x/sys/unix"
)

func TestArmCommand_DropsInheritedLoaderSettings(t *testing.T) {
	t.Setenv("LD_PRELOAD", "/evil.so")
	t.Setenv("LD_LIBRARY_PATH", "/evil")
	t.Setenv("SPX_HARDKILL_MS", "1")
	t.Setenv("KEEP_ME", "yes")
	cmd := exec.Command("/bin/true")
	cmd.ExtraFiles = []*os.File{os.Stdin}
	lib := emptyFile(t)
	hk, err := armCommand(cmd, lib, 90*time.Second)
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
	require.Equal(t, []string{"LD_PRELOAD=/proc/self/fd/5"}, loader)
	require.ElementsMatch(t, []string{"SPX_HARDKILL_MS=90000", "SPX_HARDKILL_ACK_FD=4",
		"SPX_HARDKILL_LIB_FD=5", "SPX_HARDKILL_NONCE=" + hk.nonce}, backstop)
	require.Contains(t, cmd.Env, "KEEP_ME=yes")
	require.Equal(t, []*os.File{os.Stdin, hk.w, lib}, cmd.ExtraFiles)
	require.Len(t, hk.nonce, 32)
}

func emptyFile(t *testing.T) *os.File {
	t.Helper()
	f, err := os.CreateTemp(t.TempDir(), "lib")
	require.NoError(t, err)
	t.Cleanup(func() { _ = f.Close() })
	return f
}

// runArmed runs script as the "VM" under attempt.Run with a backstop and
// reports whether the step after the ack ran.
func runArmed(t *testing.T, script string, wait time.Duration) (bool, attempt.Outcome, error) {
	t.Helper()
	cmd := exec.Command("/bin/sh", "-c", script)
	hk, err := armCommand(cmd, emptyFile(t), time.Minute)
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
	libFD := "SPX_HARDKILL_LIB_FD=4"
	cases := map[string][]string{
		"no settings":      nil,
		"bad deadline":     {"SPX_HARDKILL_MS=12x", "SPX_HARDKILL_ACK_FD=3", libFD, nonce},
		"zero deadline":    {"SPX_HARDKILL_MS=0", "SPX_HARDKILL_ACK_FD=3", libFD, nonce},
		"low fd":           {"SPX_HARDKILL_MS=1000", "SPX_HARDKILL_ACK_FD=2", libFD, nonce},
		"no lib fd":        {"SPX_HARDKILL_MS=1000", "SPX_HARDKILL_ACK_FD=3", nonce},
		"lib fd is ack fd": {"SPX_HARDKILL_MS=1000", "SPX_HARDKILL_ACK_FD=3", "SPX_HARDKILL_LIB_FD=3", nonce},
		"lib fd not open":  {"SPX_HARDKILL_MS=1000", "SPX_HARDKILL_ACK_FD=3", "SPX_HARDKILL_LIB_FD=98", nonce},
		"short nonce":      {"SPX_HARDKILL_MS=1000", "SPX_HARDKILL_ACK_FD=3", libFD, "SPX_HARDKILL_NONCE=abc"},
		"ack fd not open":  {"SPX_HARDKILL_MS=1000", "SPX_HARDKILL_ACK_FD=97", libFD, nonce},
	}
	for name, env := range cases {
		t.Run(name, func(t *testing.T) {
			marker := filepath.Join(t.TempDir(), "ran")
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, "/usr/bin/touch", marker)
			cmd.Cancel = func() error { return cmd.Process.Signal(syscall.SIGTERM) }
			cmd.Env = append([]string{"LD_PRELOAD=" + lib}, env...)
			r, w, err := os.Pipe()
			require.NoError(t, err)
			defer r.Close()
			cmd.ExtraFiles = []*os.File{w, emptyFile(t)}
			runErr := cmd.Run()
			_ = w.Close()
			ack, _ := io.ReadAll(r)
			requireSIGKILL(t, runErr)
			require.NoFileExists(t, marker)
			require.Empty(t, ack, "no ack from an initializer that failed")
		})
	}
}

// sealedBuilt builds the initializer and seals it the way the daemon does,
// pinning the digest of the bytes just built as a trusted build would.
func sealedBuilt(t *testing.T) *os.File {
	t.Helper()
	path := buildInitializer(t)
	b, err := os.ReadFile(path)
	require.NoError(t, err)
	sum := sha256.Sum256(b)
	d := &Daemon{Initializer: path, InitializerSHA256: hex.EncodeToString(sum[:])}
	lib, err := d.sealInitializer()
	require.NoError(t, err)
	t.Cleanup(func() { _ = lib.Close() })
	return lib
}

// mdweWrapper builds a launcher that applies the kernel W^X restriction
// (PR_SET_MDWE), moves MDWE_PRELOAD to LD_PRELOAD so only the exec'd
// program loads the initializer, and execs its arguments with inherited fds.
func mdweWrapper(t *testing.T) string {
	t.Helper()
	cc, err := exec.LookPath("cc")
	if err != nil {
		t.Skip("no C compiler")
	}
	dir := t.TempDir()
	src := filepath.Join(dir, "mdwe.c")
	require.NoError(t, os.WriteFile(src, []byte(`#include <sys/prctl.h>
#include <unistd.h>
#include <stdio.h>
#include <stdlib.h>
int main(int c, char **v) {
	(void)c;
	if (prctl(65, 1, 0, 0, 0) != 0) { perror("PR_SET_MDWE"); return 125; }
	if (prctl(66, 0, 0, 0, 0) != 1) { fputs("MDWE not in effect\n", stderr); return 124; }
	const char *p = getenv("MDWE_PRELOAD");
	if (!p || setenv("LD_PRELOAD", p, 1) || unsetenv("MDWE_PRELOAD")) return 123;
	execv(v[1], v + 1);
	return 126;
}
`), 0o600))
	bin := filepath.Join(dir, "mdwe")
	if out, err := exec.Command(cc, "-o", bin, src).CombinedOutput(); err != nil {
		t.Fatalf("build wrapper: %v: %s", err, out)
	}
	return bin
}

// The sealed memfd loads through /proc/self/fd, arms the timer, leaves the
// process no handle on it, and the timer kills the process at its deadline.
func TestHardKill_SealedInitializerLoadsByFD(t *testing.T) {
	launchers := map[string][]string{"plain": nil, "under PR_SET_MDWE": {mdweWrapper(t)}}
	for name, prefix := range launchers {
		t.Run(name, func(t *testing.T) {
			lib := sealedBuilt(t)
			argv := append(append([]string{}, prefix...), "/bin/sleep", "60")
			cmd := exec.Command(argv[0], argv[1:]...)
			hk, err := armCommand(cmd, lib, 400*time.Millisecond)
			require.NoError(t, err)
			defer hk.close()
			if prefix != nil {
				for i, kv := range cmd.Env {
					if strings.HasPrefix(kv, "LD_PRELOAD=") {
						cmd.Env[i] = "MDWE_" + kv[len("LD_"):]
					}
				}
			}
			require.NoError(t, cmd.Start())
			require.NoError(t, hk.await(context.Background(), cmd.Process.Pid, 5*time.Second))

			fds, err := os.ReadDir(fmt.Sprintf("/proc/%d/fd", cmd.Process.Pid))
			require.NoError(t, err)
			for _, fd := range fds {
				target, _ := os.Readlink(fmt.Sprintf("/proc/%d/fd/%s", cmd.Process.Pid, fd.Name()))
				require.NotContains(t, target, "spx-hardkill", "fd %s still open", fd.Name())
			}
			start := time.Now()
			requireSIGKILL(t, cmd.Wait())
			require.Less(t, time.Since(start), 5*time.Second)
		})
	}
}

func TestSealInitializer_SealedAgainstChange(t *testing.T) {
	lib := sealedBuilt(t)
	seals, err := unix.FcntlInt(lib.Fd(), unix.F_GET_SEALS, 0)
	require.NoError(t, err)
	require.Equal(t, requiredSeals, seals&requiredSeals)
	_, err = lib.WriteAt([]byte("x"), 0)
	require.ErrorIs(t, err, unix.EPERM)
	require.ErrorIs(t, unix.Ftruncate(int(lib.Fd()), 1), unix.EPERM)
	_, err = unix.FcntlInt(lib.Fd(), unix.F_ADD_SEALS, unix.F_SEAL_FUTURE_WRITE)
	require.ErrorIs(t, err, unix.EPERM)

	// Reopening through /proc reaches the same sealed object, not a copy.
	again, err := os.OpenFile(fmt.Sprintf("/proc/self/fd/%d", lib.Fd()), os.O_RDWR, 0)
	if err == nil {
		_, err = again.WriteAt([]byte("x"), 0)
		_ = again.Close()
	}
	require.Error(t, err)
}

// MFD_EXEC unavailable or an executable memfd refused fails closed, with no
// retry that could create a memfd the node did not allow.
func TestSealInitializer_MemfdExecRefusalsFailClosed(t *testing.T) {
	cases := map[string]struct {
		err  error
		want string
	}{
		"MFD_EXEC unavailable":    {unix.EINVAL, "MFD_EXEC unavailable or refused"},
		"executable memfd denied": {unix.EACCES, "executable memfd refused"},
		"other failure":           {unix.EMFILE, "too many open files"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			old := memfdCreate
			t.Cleanup(func() { memfdCreate = old })
			var flags []int
			memfdCreate = func(string, int) (int, error) {
				flags = append(flags, 0)
				return -1, tc.err
			}
			m, err := sealedCopy([]byte("library"))
			require.Nil(t, m)
			require.ErrorIs(t, err, tc.err)
			require.ErrorContains(t, err, tc.want)
			require.Len(t, flags, 1, "no retry")
		})
	}
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
		"empty file": func(d *Daemon) {
			empty := filepath.Join(t.TempDir(), "empty.so")
			require.NoError(t, os.WriteFile(empty, nil, 0o600))
			d.Initializer = empty
		},
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
