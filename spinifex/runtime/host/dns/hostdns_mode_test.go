//go:build unix

package hostdns

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

// withUmask sets the process umask for one test. The umask is process-wide, so
// tests using it must not run in parallel.
func withUmask(t *testing.T, mask int) {
	t.Helper()
	old := syscall.Umask(mask)
	t.Cleanup(func() { syscall.Umask(old) })
}

func assertMode(t *testing.T, path string, want os.FileMode) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	if got := info.Mode().Perm(); got != want {
		t.Errorf("%s mode = %o, want %o", path, got, want)
	}
}

// seedPrivate writes a fragment as a run under umask 077 left it: the file 0600
// and its directory 0700.
func seedPrivate(t *testing.T, path string, content []byte) {
	t.Helper()
	writeFile(t, path, content)
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
}

func TestConfigureResolvedPublicModesUnderRestrictiveUmask(t *testing.T) {
	withUmask(t, 0o077)
	c := newTestConfigurator(t)
	c.resolvedActive = func() bool { return true }
	c.resolvedStatus = func() (string, error) { return reflectingResolvedStatus(testParams), nil }

	if err := c.configure(testParams); err != nil {
		t.Fatalf("configure: %v", err)
	}
	dropin := c.path(resolvedDropinPath)
	assertMode(t, dropin, 0o644)
	assertMode(t, filepath.Dir(dropin), 0o755)
}

func TestConfigureResolvedRewritesUnreadableDropin(t *testing.T) {
	withUmask(t, 0o077)
	c := newTestConfigurator(t)
	c.resolvedActive = func() bool { return true }
	restarts := 0
	c.restartResolved = func() error { restarts++; return nil }
	// resolved ignored the unreadable drop-in until it is restarted.
	c.resolvedStatus = func() (string, error) {
		if restarts == 0 {
			return "Global\n  DNS Servers: 169.254.169.254\n", nil
		}
		return reflectingResolvedStatus(testParams), nil
	}
	dropin := c.path(resolvedDropinPath)
	seedPrivate(t, dropin, renderResolvedDropin(testParams))

	if err := c.configure(testParams); err != nil {
		t.Fatalf("configure: %v", err)
	}
	if restarts != 1 {
		t.Errorf("restartResolved called %d times, want 1", restarts)
	}
	assertMode(t, dropin, 0o644)
	assertMode(t, filepath.Dir(dropin), 0o755)
}

func TestConfigureResolvedFixesModesWithoutRestart(t *testing.T) {
	withUmask(t, 0o077)
	c := newTestConfigurator(t)
	c.resolvedActive = func() bool { return true }
	c.restartResolved = func() error { t.Fatalf("restart on an already-current host"); return nil }
	c.resolvedStatus = func() (string, error) { return reflectingResolvedStatus(testParams), nil }
	dropin := c.path(resolvedDropinPath)
	seedPrivate(t, dropin, renderResolvedDropin(testParams))

	if err := c.configure(testParams); err != nil {
		t.Fatalf("configure: %v", err)
	}
	assertMode(t, dropin, 0o644)
	assertMode(t, filepath.Dir(dropin), 0o755)
}

func TestConfigureResolvconfPublicModesUnderRestrictiveUmask(t *testing.T) {
	withUmask(t, 0o077)
	c := newTestConfigurator(t)
	c.hasResolvconf = func() bool { return true }
	writeFile(t, c.path(resolvConfPath), []byte("nameserver 10.0.0.5\n"))

	if err := c.configure(testParams); err != nil {
		t.Fatalf("configure: %v", err)
	}
	head := c.path(resolvconfHeadPath)
	assertMode(t, head, 0o644)
	assertMode(t, filepath.Dir(head), 0o755)
}

func TestConfigureResolvconfFixesModesWithoutUpdate(t *testing.T) {
	withUmask(t, 0o077)
	c := newTestConfigurator(t)
	c.hasResolvconf = func() bool { return true }
	c.updateResolvconf = func() error { t.Fatalf("resolvconf -u on an already-current host"); return nil }
	head := c.path(resolvconfHeadPath)
	seedPrivate(t, head, renderResolvconfHead(testParams))
	writeFile(t, c.path(resolvConfPath), []byte("nameserver 10.0.0.5\n"))

	if err := c.configure(testParams); err != nil {
		t.Fatalf("configure: %v", err)
	}
	assertMode(t, head, 0o644)
	assertMode(t, filepath.Dir(head), 0o755)
}
