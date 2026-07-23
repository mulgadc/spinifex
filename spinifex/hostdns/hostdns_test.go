package hostdns

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// testParams are the values admin init/join would pass: the node-local northstar
// listener and the two default Spinifex zones.
var testParams = Params{
	ResolverIP:     "10.0.0.5",
	BaseDomain:     "spx3.net",
	InternalDomain: "compute.internal",
}

// newTestConfigurator returns a configurator rooted at a temp dir with every
// side effect stubbed to a no-op. Individual tests override the fields they
// exercise.
func newTestConfigurator(t *testing.T) *configurator {
	t.Helper()
	return &configurator{
		root:             t.TempDir(),
		resolvedActive:   func() bool { return false },
		hasResolvconf:    func() bool { return false },
		restartResolved:  func() error { return nil },
		updateResolvconf: func() error { return nil },
		resolvedStatus:   func() (string, error) { return "", nil },
	}
}

// reflectingResolvedStatus returns resolvectl output that reflects the drop-in,
// so verification passes.
func reflectingResolvedStatus(p Params) string {
	return "Global\n" +
		"       DNS Servers: " + p.ResolverIP + "\n" +
		"        DNS Domain: ~" + p.BaseDomain + " ~" + p.InternalDomain + "\n"
}

func TestConfigureResolvedWritesRouteOnlyDropin(t *testing.T) {
	c := newTestConfigurator(t)
	c.resolvedActive = func() bool { return true }
	restarts := 0
	c.restartResolved = func() error { restarts++; return nil }
	c.resolvedStatus = func() (string, error) { return reflectingResolvedStatus(testParams), nil }

	if err := c.configure(testParams); err != nil {
		t.Fatalf("configure: %v", err)
	}
	if restarts != 1 {
		t.Errorf("restartResolved called %d times, want 1", restarts)
	}

	got := readFile(t, c.path(resolvedDropinPath))
	// DNS= is the node listener and the "~" prefix scopes it to the Spinifex
	// zones — all other names keep the link DNS.
	for _, want := range []string{
		"[Resolve]\n",
		"DNS=10.0.0.5\n",
		"Domains=~spx3.net ~compute.internal\n",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("drop-in missing %q\ngot:\n%s", want, got)
		}
	}
	// A resolvconf host is not touched when resolved owns the resolver.
	if _, err := os.Stat(c.path(resolvconfHeadPath)); !os.IsNotExist(err) {
		t.Errorf("resolvconf head written on a resolved host")
	}
}

func TestConfigureResolvedIdempotentSkipsRestart(t *testing.T) {
	c := newTestConfigurator(t)
	c.resolvedActive = func() bool { return true }
	c.restartResolved = func() error { t.Fatalf("restart on an already-current host"); return nil }
	c.resolvedStatus = func() (string, error) { return reflectingResolvedStatus(testParams), nil }

	// Pre-seed the exact drop-in the render would produce.
	writeFile(t, c.path(resolvedDropinPath), renderResolvedDropin(testParams))

	if err := c.configure(testParams); err != nil {
		t.Fatalf("configure: %v", err)
	}
}

func TestConfigureResolvedVerificationFailureSurfaced(t *testing.T) {
	c := newTestConfigurator(t)
	c.resolvedActive = func() bool { return true }
	restarts := 0
	c.restartResolved = func() error { restarts++; return nil }
	// resolved comes back without the node as a DNS server — the change did not
	// take, so success must not be reported.
	c.resolvedStatus = func() (string, error) { return "Global\n  DNS Servers: 127.0.0.53\n", nil }

	err := c.configure(testParams)
	if err == nil {
		t.Fatal("expected verification error, got nil")
	}
	if !strings.Contains(err.Error(), "does not list") {
		t.Errorf("error = %v, want a DNS-server verification failure", err)
	}
	if restarts != 1 {
		t.Errorf("restartResolved called %d times, want 1", restarts)
	}
}

func TestConfigureResolvconfNorthstarFirst(t *testing.T) {
	c := newTestConfigurator(t)
	c.hasResolvconf = func() bool { return true }
	updates := 0
	c.updateResolvconf = func() error {
		updates++
		// resolvconf -u prepends the head fragment ahead of the upstream servers.
		regen := "nameserver 10.0.0.5\nnameserver 8.8.8.8\n"
		return os.WriteFile(c.path(resolvConfPath), []byte(regen), 0o644)
	}

	if err := c.configure(testParams); err != nil {
		t.Fatalf("configure: %v", err)
	}
	if updates != 1 {
		t.Errorf("updateResolvconf called %d times, want 1", updates)
	}

	head := readFile(t, c.path(resolvconfHeadPath))
	if !strings.Contains(head, "nameserver 10.0.0.5\n") {
		t.Errorf("head fragment missing northstar nameserver\ngot:\n%s", head)
	}
}

func TestConfigureResolvconfIdempotentSkipsUpdate(t *testing.T) {
	c := newTestConfigurator(t)
	c.hasResolvconf = func() bool { return true }
	c.updateResolvconf = func() error { t.Fatalf("resolvconf -u on an already-current host"); return nil }

	writeFile(t, c.path(resolvconfHeadPath), renderResolvconfHead(testParams))
	writeFile(t, c.path(resolvConfPath), []byte("nameserver 10.0.0.5\nnameserver 8.8.8.8\n"))

	if err := c.configure(testParams); err != nil {
		t.Fatalf("configure: %v", err)
	}
}

func TestConfigurePrefersResolvedOverResolvconf(t *testing.T) {
	c := newTestConfigurator(t)
	c.resolvedActive = func() bool { return true }
	c.hasResolvconf = func() bool { return true }
	c.updateResolvconf = func() error { t.Fatalf("resolvconf touched while resolved is active"); return nil }
	c.resolvedStatus = func() (string, error) { return reflectingResolvedStatus(testParams), nil }

	if err := c.configure(testParams); err != nil {
		t.Fatalf("configure: %v", err)
	}
	if _, err := os.Stat(c.path(resolvedDropinPath)); err != nil {
		t.Errorf("resolved drop-in not written: %v", err)
	}
}

func TestConfigureNoSupportedManagerErrors(t *testing.T) {
	c := newTestConfigurator(t) // both managers absent by default

	err := c.configure(testParams)
	if err == nil {
		t.Fatal("expected an error when no resolver manager is present")
	}
	// The error must name the manual remediation rather than skip silently.
	for _, want := range []string{"no supported resolver manager", "10.0.0.5:53"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error = %v, want it to contain %q", err, want)
		}
	}
}

func TestConfigureRejectsUnusableResolverIP(t *testing.T) {
	for _, ip := range []string{"", "0.0.0.0"} {
		c := newTestConfigurator(t)
		c.resolvedActive = func() bool { t.Fatalf("manager probed before validation"); return false }

		p := testParams
		p.ResolverIP = ip
		if err := c.configure(p); err == nil {
			t.Errorf("ResolverIP %q: expected validation error, got nil", ip)
		}
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(data)
}

func writeFile(t *testing.T, path string, content []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, content, 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}
