// Package hostdns points the node's own host resolver at the local northstar
// listener so the Spinifex authoritative zones (ELB/EKS names in the base zone
// and the AWS-parity private zone) resolve from the node itself, not just from
// guest VMs handed northstar over DHCP.
//
// It is called from the universal chokepoints that write the node's
// northstar.toml — spx admin init and spx admin join — fed straight from the
// in-memory settings that render that file, so nothing is re-parsed off disk. It
// configures whichever resolver manager the host runs: an active
// systemd-resolved gets a route-only drop-in that sends only the Spinifex zones
// to northstar while every other name keeps the link DNS; otherwise resolvconf
// gets northstar as the first nameserver ahead of the retained upstream servers.
// A host with neither manager is an error naming the manual remediation, never a
// silent skip.
package hostdns

import (
	"bufio"
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// Host paths the resolver managers read. Joined onto configurator.root, which is
// empty in production and a temp dir under test.
const (
	resolvedDropinPath = "/etc/systemd/resolved.conf.d/spinifex-dns.conf"
	resolvconfHeadPath = "/etc/resolvconf/resolv.conf.d/head"
	resolvConfPath     = "/etc/resolv.conf"
)

// Params carries the node-local resolver target and the two Spinifex zones that
// must resolve through it. All three come straight from the admin.ConfigSettings
// that also render northstar.toml, so the listener is never re-parsed off disk.
type Params struct {
	// ResolverIP is the node-local northstar listener (the node AdvertiseIP,
	// falling back to BindIP). It becomes DNS= / the first nameserver directly.
	ResolverIP string
	// BaseDomain is the authoritative base zone (e.g. "spx3.net").
	BaseDomain string
	// InternalDomain is the AWS-parity private zone (e.g. "compute.internal").
	InternalDomain string
}

// validate rejects params that would produce a broken resolver — an unset or
// wildcard IP cannot be dialled, and the domains anchor the routing rules.
func (p Params) validate() error {
	switch {
	case p.ResolverIP == "" || p.ResolverIP == "0.0.0.0":
		return fmt.Errorf("resolver IP is unset (advertise/bind IP was %q); cannot point the host resolver at northstar", p.ResolverIP)
	case p.BaseDomain == "":
		return fmt.Errorf("base domain is empty")
	case p.InternalDomain == "":
		return fmt.Errorf("internal domain is empty")
	}
	return nil
}

// Configure points this host's resolver at northstar for the Spinifex zones.
// It is idempotent (no resolver restart when the fragment and the running
// resolver already reflect the target) and verifies the change against the live
// resolver before returning nil.
func Configure(p Params) error {
	return newConfigurator().configure(p)
}

// configurator holds the resolver-manager side effects behind function fields so
// the render, idempotency, and verification logic runs under go test with
// stubbed commands and a rooted temp filesystem.
type configurator struct {
	// root is prefixed onto every host path; "" in production.
	root string

	// resolvedActive reports whether systemd-resolved is the active resolver.
	resolvedActive func() bool
	// hasResolvconf reports whether the resolvconf binary is on PATH.
	hasResolvconf func() bool

	// restartResolved reloads systemd-resolved after a drop-in change.
	restartResolved func() error
	// updateResolvconf regenerates /etc/resolv.conf from its fragments.
	updateResolvconf func() error

	// resolvedStatus returns `resolvectl status` output for verification.
	resolvedStatus func() (string, error)
}

// newConfigurator wires the production side effects. admin init/join run as root,
// so the writes to /etc and the systemctl/resolvconf calls have privileges.
func newConfigurator() *configurator {
	return &configurator{
		resolvedActive: func() bool {
			return exec.Command("systemctl", "is-active", "--quiet", "systemd-resolved").Run() == nil
		},
		hasResolvconf: func() bool {
			_, err := exec.LookPath("resolvconf")
			return err == nil
		},
		restartResolved: func() error {
			return exec.Command("systemctl", "restart", "systemd-resolved").Run()
		},
		updateResolvconf: func() error {
			return exec.Command("resolvconf", "-u").Run()
		},
		resolvedStatus: func() (string, error) {
			out, err := exec.Command("resolvectl", "status").CombinedOutput()
			return string(out), err
		},
	}
}

// configure selects the host's resolver manager and applies the northstar route.
// systemd-resolved wins when active because its route-only drop-in is the least
// disruptive; otherwise resolvconf's head fragment prepends northstar.
func (c *configurator) configure(p Params) error {
	if err := p.validate(); err != nil {
		return err
	}
	switch {
	case c.resolvedActive():
		return c.configureResolved(p)
	case c.hasResolvconf():
		return c.configureResolvconf(p)
	default:
		return fmt.Errorf(
			"no supported resolver manager (systemd-resolved or resolvconf) is present; "+
				"manually forward the Spinifex zones %s and %s to %s:53",
			p.BaseDomain, p.InternalDomain, p.ResolverIP)
	}
}

// configureResolved writes the route-only drop-in and restarts systemd-resolved.
// The restart is skipped when the file is unchanged and the running resolver
// already routes the zones to northstar; otherwise the change is verified.
func (c *configurator) configureResolved(p Params) error {
	dropin := renderResolvedDropin(p)
	dropinPath := c.path(resolvedDropinPath)

	if existing, err := os.ReadFile(dropinPath); err == nil && bytes.Equal(existing, dropin) {
		if c.verifyResolved(p) == nil {
			return nil
		}
	}

	if err := writeFragment(dropinPath, dropin); err != nil {
		return err
	}
	if err := c.restartResolved(); err != nil {
		return fmt.Errorf("restart systemd-resolved: %w", err)
	}
	return c.verifyResolved(p)
}

// verifyResolved confirms systemd-resolved reflects the drop-in: northstar is a
// DNS server and the base zone is one of its routing domains.
func (c *configurator) verifyResolved(p Params) error {
	out, err := c.resolvedStatus()
	if err != nil {
		return fmt.Errorf("query resolvectl status: %w", err)
	}
	if !strings.Contains(out, p.ResolverIP) {
		return fmt.Errorf("systemd-resolved does not list %s as a DNS server after configuration", p.ResolverIP)
	}
	// Domains render as "~spx3.net"; a bare-domain substring match tolerates the
	// prefix and resolvectl's per-version formatting.
	if !strings.Contains(out, p.BaseDomain) {
		return fmt.Errorf("systemd-resolved does not route %s to %s after configuration", p.BaseDomain, p.ResolverIP)
	}
	return nil
}

// configureResolvconf writes the head fragment and regenerates resolv.conf. The
// regeneration is skipped when the fragment is unchanged and northstar is
// already the first nameserver; otherwise the result is verified.
func (c *configurator) configureResolvconf(p Params) error {
	head := renderResolvconfHead(p)
	headPath := c.path(resolvconfHeadPath)

	if existing, err := os.ReadFile(headPath); err == nil && bytes.Equal(existing, head) {
		if c.verifyResolvconf(p) == nil {
			return nil
		}
	}

	if err := writeFragment(headPath, head); err != nil {
		return err
	}
	if err := c.updateResolvconf(); err != nil {
		return fmt.Errorf("resolvconf -u: %w", err)
	}
	return c.verifyResolvconf(p)
}

// verifyResolvconf confirms the regenerated resolv.conf queries northstar first,
// ahead of the upstream servers resolvconf keeps as fallbacks.
func (c *configurator) verifyResolvconf(p Params) error {
	first, err := firstNameserver(c.path(resolvConfPath))
	if err != nil {
		return fmt.Errorf("read %s: %w", resolvConfPath, err)
	}
	if first != p.ResolverIP {
		return fmt.Errorf("%s: first nameserver is %q, expected node resolver %s", resolvConfPath, first, p.ResolverIP)
	}
	return nil
}

// path roots a host path under c.root (a no-op in production, a temp dir in tests).
func (c *configurator) path(p string) string {
	return filepath.Join(c.root, p)
}

// renderResolvedDropin builds the resolved.conf.d drop-in. The "~" routing-domain
// prefix scopes DNS= to exactly the Spinifex zones, so every other name keeps
// using the link/DHCP DNS.
func renderResolvedDropin(p Params) []byte {
	return fmt.Appendf(nil, `# Generated by spx admin init/join — route the Spinifex authoritative zones to
# the node-local northstar resolver. The "~" routing-domain prefix restricts
# this DNS= server to exactly these zones; all other names use the link DNS.
[Resolve]
DNS=%s
Domains=~%s ~%s
`, p.ResolverIP, p.BaseDomain, p.InternalDomain)
}

// renderResolvconfHead builds the resolvconf head fragment. It is prepended
// verbatim to the generated resolv.conf, ahead of the upstream servers, so
// northstar is queried first while those upstreams remain as fallbacks.
func renderResolvconfHead(p Params) []byte {
	return fmt.Appendf(nil, `# Generated by spx admin init/join — query the node-local northstar resolver
# first so the Spinifex authoritative zones (ELB/EKS names) resolve from this
# node. The upstream servers below remain as fallbacks.
nameserver %s
`, p.ResolverIP)
}

// writeFragment creates the parent directory and writes a resolver fragment.
// Resolver config is public: the drop-in dir must be world-traversable (0755)
// and the fragment world-readable (0644) so the resolver daemon and every
// name-resolving process can read them, matching distro defaults for
// /etc/systemd/resolved.conf.d and /etc/resolvconf.
func writeFragment(path string, content []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil { //nolint:gosec // resolver config dir is intentionally world-traversable
		return fmt.Errorf("create %s: %w", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, content, 0o644); err != nil { //nolint:gosec // resolver fragment is intentionally world-readable
		return fmt.Errorf("write %s: %w", path, err)
	}
	return nil
}

// firstNameserver returns the first "nameserver" entry in a resolv.conf, or ""
// when the file has none.
func firstNameserver(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) >= 2 && fields[0] == "nameserver" {
			return fields[1], nil
		}
	}
	return "", sc.Err()
}
