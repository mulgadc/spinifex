//go:build linux

package agent

import (
	"bytes"
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	goruntime "runtime"
	"strings"
	"syscall"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// statusProbe writes an executable that prints its own credential lines.
func statusProbe(t *testing.T) string {
	t.Helper()
	// The sandbox uid must be able to reach the probe through the test's dirs.
	dir := t.TempDir()
	require.NoError(t, os.Chmod(filepath.Dir(dir), 0o755))
	require.NoError(t, os.Chmod(dir, 0o755))
	path := filepath.Join(dir, "bootstrap")
	script := "#!/bin/sh\ngrep -E '^(Uid|Gid|Groups|NoNewPrivs):' /proc/self/status\n"
	require.NoError(t, os.WriteFile(path, []byte(script), 0o755))
	return path
}

func statusLines(t *testing.T, out []byte) map[string]string {
	t.Helper()
	lines := map[string]string{}
	for l := range strings.SplitSeq(strings.TrimSpace(string(out)), "\n") {
		k, v, ok := strings.Cut(l, ":")
		require.True(t, ok, "status line %q", l)
		lines[k] = strings.Join(strings.Fields(v), " ")
	}
	return lines
}

// run starts a bootstrap with the given credential and returns its stdout.
func run(t *testing.T, path string, env []string, sandbox bool) []byte {
	t.Helper()
	var out bytes.Buffer
	start := startBootstrap
	if sandbox {
		start = func(ctx context.Context, p string, e []string, o, s io.Writer, _ *syscall.Credential) (*exec.Cmd, error) {
			return StartBootstrap(ctx, p, e, o, s)
		}
	}
	cmd, err := start(context.Background(), path, env, &out, os.Stderr, nil)
	require.NoError(t, err)
	require.NoError(t, cmd.Wait())
	return out.Bytes()
}

// threadNoNewPrivs reads no_new_privs for the calling OS thread.
func threadNoNewPrivs(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile("/proc/thread-self/status")
	require.NoError(t, err)
	return statusLines(t, grepLines(b, "NoNewPrivs:"))["NoNewPrivs"]
}

func grepLines(b []byte, prefix string) []byte {
	var keep []string
	for l := range strings.SplitSeq(string(b), "\n") {
		if strings.HasPrefix(l, prefix) {
			keep = append(keep, l)
		}
	}
	return []byte(strings.Join(keep, "\n"))
}

func TestStartBootstrapSetsNoNewPrivsWithoutChangingTheCaller(t *testing.T) {
	goruntime.LockOSThread()
	defer goruntime.UnlockOSThread()
	if threadNoNewPrivs(t) != "0" {
		t.Skip("test thread already has no_new_privs; the child would inherit it")
	}

	out := run(t, statusProbe(t), nil, false)
	assert.Equal(t, "1", statusLines(t, out)["NoNewPrivs"])
	assert.Equal(t, "0", threadNoNewPrivs(t), "the calling thread must keep no_new_privs unset")
}

func TestStartBootstrapDropsToSandboxIdentity(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("dropping to the sandbox uid needs root")
	}
	lines := statusLines(t, run(t, statusProbe(t), nil, true))
	assert.Equal(t, "993 993 993 993", lines["Uid"])
	assert.Equal(t, "993 993 993 993", lines["Gid"])
	assert.Empty(t, lines["Groups"])
	assert.Equal(t, "1", lines["NoNewPrivs"])
}

// A tenant LD_PRELOAD constructor runs before the bootstrap's main; it must
// already see no_new_privs, so no loader runs before the flag is set.
func TestStartBootstrapPreloadConstructorSeesNoNewPrivs(t *testing.T) {
	goruntime.LockOSThread()
	defer goruntime.UnlockOSThread()
	if threadNoNewPrivs(t) != "0" {
		t.Skip("test thread already has no_new_privs; the check would pass vacuously")
	}
	cc, err := exec.LookPath("cc")
	if err != nil {
		t.Skip("no C compiler for the preload library")
	}
	bootstrap, err := exec.LookPath("true")
	require.NoError(t, err)
	dir := t.TempDir()
	src := filepath.Join(dir, "preload.c")
	lib := filepath.Join(dir, "preload.so")
	require.NoError(t, os.WriteFile(src, []byte(preloadSource), 0o644))
	build := exec.Command(cc, "-shared", "-fPIC", "-o", lib, src)
	build.Stderr = os.Stderr
	require.NoError(t, build.Run())

	out := run(t, bootstrap, []string{"LD_PRELOAD=" + lib}, false)
	assert.Equal(t, "preload NoNewPrivs: 1", strings.Join(strings.Fields(string(out)), " "))
}

const preloadSource = `#include <stdio.h>
#include <string.h>

__attribute__((constructor)) static void report(void) {
	char line[256];
	FILE *f = fopen("/proc/self/status", "r");
	if (!f)
		return;
	while (fgets(line, sizeof line, f))
		if (strncmp(line, "NoNewPrivs:", 11) == 0)
			printf("preload %s", line);
	fclose(f);
	fflush(stdout);
}
`

func TestStartBootstrapDoesNotInheritAgentEnvironment(t *testing.T) {
	t.Setenv("ZZ_AGENT_SECRET", "must-not-leak")
	probe, err := exec.LookPath("env")
	require.NoError(t, err)

	for name, env := range map[string][]string{"nil": nil, "empty": {}, "given": {"ONLY=this"}} {
		out := run(t, probe, env, false)
		// Never print the captured environment: an inherited one may hold secrets.
		if strings.TrimSuffix(string(out), "\n") != strings.Join(env, "\n") {
			t.Errorf("%s: bootstrap environment is not exactly the one given", name)
		}
	}
}

// Without root the sandbox credential cannot be applied, so the exported entry
// point must refuse rather than run the bootstrap as the caller.
func TestStartBootstrapAsNonRootRefusesInsteadOfKeepingCallerIdentity(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root can apply the sandbox credential")
	}
	_, err := StartBootstrap(context.Background(), statusProbe(t), nil, nil, nil)
	assert.ErrorIs(t, err, syscall.EPERM)
}

func TestStartBootstrapMissingExecutableFails(t *testing.T) {
	_, err := startBootstrap(context.Background(), filepath.Join(t.TempDir(), "bootstrap"), nil, nil, nil, nil)
	assert.Error(t, err)
}

func TestStartBootstrapRunsInItsOwnDirectory(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "data.txt"), []byte("packaged"), 0o644))
	bootstrap := filepath.Join(dir, "bootstrap")
	require.NoError(t, os.WriteFile(bootstrap, []byte("#!/bin/sh\ncat data.txt\n"), 0o755))
	assert.Equal(t, "packaged", string(run(t, bootstrap, []string{"PATH=/bin:/usr/bin"}, false)))
}
