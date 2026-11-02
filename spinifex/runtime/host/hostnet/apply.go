package hostnet

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

const (
	// DefaultLinkWait bounds the wait for links other components create: OVS
	// bridges, and NetworkManager's uplink bridge and NICs.
	DefaultLinkWait = 60 * time.Second
	// DefaultLockWait bounds the wait behind setup-ovn.sh, so a stuck setup
	// cannot hang boot.
	DefaultLockWait = 120 * time.Second
	pollInterval    = 500 * time.Millisecond
)

// Env is everything Apply and Check touch on the host.
type Env struct {
	Paths    Paths
	Host     Host
	LinkWait time.Duration
	LockWait time.Duration
	// OVNRemote returns external_ids:ovn-remote, or "" when unset.
	OVNRemote func(ctx context.Context) (string, error)
	// Sleep waits between polls; tests replace it to advance a fake clock.
	Sleep func(time.Duration)
	Now   func() time.Time
}

// DefaultEnv returns the Env for a live host.
func DefaultEnv() Env {
	return Env{
		Paths:     DefaultPaths(),
		Host:      NewIPHost(),
		LinkWait:  DefaultLinkWait,
		LockWait:  DefaultLockWait,
		OVNRemote: func(ctx context.Context) (string, error) { return ovnRemote(ctx, execRunner{}) },
		Sleep:     time.Sleep,
		Now:       time.Now,
	}
}

// resolve decides what this node needs: nil state means nothing to apply.
// Apply and Check share it, so every case that fails Apply fails Check.
func resolve(ctx context.Context, env Env) (*State, error) {
	backend, err := ReadBackend(env.Paths.Backend)
	if err != nil {
		return nil, err
	}
	switch backend {
	case BackendNetworkd:
		return nil, nil
	case BackendHostnet:
		return LoadState(env.Paths.State)
	}

	// No record: a node set up before the record existed has networkd files.
	legacy, err := hasNetworkdFiles(env.Paths.NetworkdDir)
	if err != nil {
		return nil, fmt.Errorf("look for networkd files in %s: %w", env.Paths.NetworkdDir, err)
	}
	if legacy {
		return nil, nil
	}
	remote, err := env.OVNRemote(ctx)
	if err != nil {
		return nil, fmt.Errorf("no backend record at %s, and cannot tell whether this node is set up: %w", env.Paths.Backend, err)
	}
	if remote != "" {
		return nil, fmt.Errorf("this node is set up (external_ids:ovn-remote=%s) but has no backend record at %s and no networkd files; rerun setup-ovn.sh", remote, env.Paths.Backend)
	}
	return nil, nil
}

// Check reports whether the daemon and vpcd may start: the same decision as
// Apply, plus, on the hostnet backend, proof the last Apply succeeded.
func Check(ctx context.Context, env Env) error {
	s, err := resolve(ctx, env)
	if err != nil {
		return err
	}
	if s == nil {
		return nil
	}
	if _, err := os.Stat(env.Paths.Marker); err != nil {
		return fmt.Errorf("host network not applied (no %s): see `systemctl status spinifex-hostnet` and `journalctl -u spinifex-hostnet`", env.Paths.Marker)
	}
	return nil
}

// Apply makes the host match the state file, under the hostnet lock. It
// removes the readiness marker first and writes it only on success.
func Apply(ctx context.Context, env Env) error {
	unlock, err := acquireLock(env)
	if err != nil {
		return err
	}
	defer unlock()

	if err := os.Remove(env.Paths.Marker); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove %s: %w", env.Paths.Marker, err)
	}
	s, err := resolve(ctx, env)
	if err != nil {
		return err
	}
	if s == nil {
		slog.Info("hostnet: nothing to apply on this node")
		return nil
	}
	if err := reconcile(ctx, env, s); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(env.Paths.Marker), 0o770); err != nil { //nolint:gosec // G301: the mode setup.sh's tmpfiles entry gives /run/spinifex, which the daemon and vpcd traverse to check the marker. Holds no secret material.
		return fmt.Errorf("create %s: %w", filepath.Dir(env.Paths.Marker), err)
	}
	stamp := env.Now().UTC().Format(time.RFC3339) + "\n"
	if err := os.WriteFile(env.Paths.Marker, []byte(stamp), 0o600); err != nil {
		return fmt.Errorf("write %s: %w", env.Paths.Marker, err)
	}
	slog.Info("hostnet: host network applied", "state", env.Paths.State)
	return nil
}

func reconcile(ctx context.Context, env Env, s *State) error {
	if err := waitForLinks(ctx, env, s); err != nil {
		return err
	}
	h := env.Host
	if err := ensureUp(ctx, h, s.IntBridge); err != nil {
		return err
	}
	if w := s.WAN; w != nil {
		if err := ensureUp(ctx, h, w.OVSBridge); err != nil {
			return err
		}
		switch w.Mode {
		case ModeVeth:
			if err := ensureVethPair(ctx, h, w.HostEnd, w.OVSEnd); err != nil {
				return err
			}
			if err := ensureMaster(ctx, h, w.HostEnd, w.LinuxBridge); err != nil {
				return err
			}
		case ModeNAT:
			if err := ensureVethPair(ctx, h, w.HostEnd, w.OVSEnd); err != nil {
				return err
			}
			if err := ensureMAC(ctx, h, w.HostEnd, w.HostMAC); err != nil {
				return err
			}
			if err := ensureAddr(ctx, h, w.HostEnd, w.HostCIDR); err != nil {
				return err
			}
		case ModeDirect:
			if err := ensurePortNIC(ctx, h, w.NIC); err != nil {
				return err
			}
		}
		if w.Mode == ModeVeth || w.Mode == ModeNAT {
			for _, end := range []string{w.HostEnd, w.OVSEnd} {
				if err := ensureUp(ctx, h, end); err != nil {
					return err
				}
			}
		}
	}
	if m := s.Mgmt; m != nil {
		if err := ensureUp(ctx, h, m.Bridge); err != nil {
			return err
		}
		if m.CIDR != "" {
			if err := ensureAddr(ctx, h, m.Bridge, m.CIDR); err != nil {
				return err
			}
		}
		if m.NIC != "" {
			if err := ensurePortNIC(ctx, h, m.NIC); err != nil {
				return err
			}
		}
	}
	if r := s.EncapRoute; r != nil {
		if err := ensureRoute(ctx, h, *r); err != nil {
			return err
		}
	}
	return nil
}

// waitForLinks waits for the links other components create and the recorded
// topology needs. The veth pairs are not waited for: Apply creates them.
func waitForLinks(ctx context.Context, env Env, s *State) error {
	needed := []string{s.IntBridge}
	if w := s.WAN; w != nil {
		needed = append(needed, w.OVSBridge)
		switch w.Mode {
		case ModeVeth:
			needed = append(needed, w.LinuxBridge)
		case ModeDirect:
			needed = append(needed, w.NIC)
		}
	}
	if m := s.Mgmt; m != nil {
		needed = append(needed, m.Bridge)
		if m.NIC != "" {
			needed = append(needed, m.NIC)
		}
	}
	if r := s.EncapRoute; r != nil {
		needed = append(needed, r.Dev)
	}
	needed = slices.Compact(slices.Sorted(slices.Values(needed)))

	deadline := env.Now().Add(env.LinkWait)
	for {
		var missing []string
		for _, name := range needed {
			l, err := env.Host.Link(ctx, name)
			if err != nil {
				return err
			}
			if l == nil {
				missing = append(missing, name)
			}
		}
		// The encap route's source address must be up before the route can
		// name it; on a DHCP uplink it can arrive after the link does.
		if len(missing) == 0 && s.EncapRoute != nil {
			has, err := hasAddr(ctx, env.Host, s.EncapRoute.Dev, s.EncapRoute.Src)
			if err != nil {
				return err
			}
			if !has {
				missing = append(missing, s.EncapRoute.Src+" on "+s.EncapRoute.Dev)
			}
		}
		if len(missing) == 0 {
			return nil
		}
		if !env.Now().Before(deadline) {
			return fmt.Errorf("still missing after %s: %s", env.LinkWait, strings.Join(missing, ", "))
		}
		env.Sleep(pollInterval)
		if err := ctx.Err(); err != nil {
			return err
		}
	}
}

func ensureUp(ctx context.Context, h Host, name string) error {
	l, err := h.Link(ctx, name)
	if err != nil {
		return err
	}
	if l == nil {
		return fmt.Errorf("%s: link missing", name)
	}
	if l.Up {
		return nil
	}
	if err := h.SetUp(ctx, name); err != nil {
		return fmt.Errorf("set %s up: %w", name, err)
	}
	slog.Info("hostnet: set link up", "link", name)
	return nil
}

// ensureVethPair creates the pair when neither end exists, and refuses any
// other link already holding either name.
func ensureVethPair(ctx context.Context, h Host, a, b string) error {
	la, err := h.Link(ctx, a)
	if err != nil {
		return err
	}
	lb, err := h.Link(ctx, b)
	if err != nil {
		return err
	}
	if la == nil && lb == nil {
		if err := h.AddVethPair(ctx, a, b); err != nil {
			return fmt.Errorf("create veth pair %s/%s: %w", a, b, err)
		}
		slog.Info("hostnet: created veth pair", "link", a, "peer", b)
		return nil
	}
	for _, l := range []*Link{la, lb} {
		if l != nil && l.Kind != "veth" {
			return fmt.Errorf("refusing to use %s: it is a %s link, not one end of the %s/%s veth pair", l.Name, kindName(l.Kind), a, b)
		}
	}
	if la == nil || lb == nil || la.Peer != b || lb.Peer != a {
		return fmt.Errorf("refusing to use veth pair %s/%s: %s", a, b, describePeers(a, b, la, lb))
	}
	return nil
}

func describePeers(a, b string, la, lb *Link) string {
	switch {
	case la == nil:
		return fmt.Sprintf("%s is missing and %s is peered with %q", a, b, lb.Peer)
	case lb == nil:
		return fmt.Sprintf("%s is missing and %s is peered with %q", b, a, la.Peer)
	default:
		return fmt.Sprintf("%s is peered with %q and %s with %q", a, la.Peer, b, lb.Peer)
	}
}

func kindName(kind string) string {
	if kind == "" {
		return "physical"
	}
	return kind
}

func ensureMaster(ctx context.Context, h Host, name, master string) error {
	l, err := h.Link(ctx, name)
	if err != nil {
		return err
	}
	if l == nil {
		return fmt.Errorf("%s: link missing", name)
	}
	if l.Master == master {
		return nil
	}
	if err := h.SetMaster(ctx, name, master); err != nil {
		return fmt.Errorf("attach %s to %s: %w", name, master, err)
	}
	slog.Info("hostnet: attached link to bridge", "link", name, "bridge", master)
	return nil
}

func ensureMAC(ctx context.Context, h Host, name, mac string) error {
	l, err := h.Link(ctx, name)
	if err != nil {
		return err
	}
	if l == nil {
		return fmt.Errorf("%s: link missing", name)
	}
	if strings.EqualFold(l.MAC, mac) {
		return nil
	}
	if err := h.SetMAC(ctx, name, mac); err != nil {
		return fmt.Errorf("set %s MAC %s: %w", name, mac, err)
	}
	slog.Info("hostnet: set MAC", "link", name, "mac", mac)
	return nil
}

func ensureAddr(ctx context.Context, h Host, name, cidr string) error {
	addrs, err := h.IPv4Addrs(ctx, name)
	if err != nil {
		return err
	}
	if slices.Contains(addrs, cidr) {
		return nil
	}
	if err := h.AddAddr(ctx, name, cidr); err != nil {
		return fmt.Errorf("add %s to %s: %w", cidr, name, err)
	}
	slog.Info("hostnet: added address", "link", name, "cidr", cidr)
	return nil
}

func hasAddr(ctx context.Context, h Host, name, ip string) (bool, error) {
	addrs, err := h.IPv4Addrs(ctx, name)
	if err != nil {
		return false, err
	}
	for _, a := range addrs {
		if p, err := netip.ParsePrefix(a); err == nil && p.Addr().String() == ip {
			return true, nil
		}
	}
	return false, nil
}

// ensurePortNIC brings up a NIC that is an OVS port and strips any IPv4
// address NetworkManager restored to it; the address belongs on the bridge.
func ensurePortNIC(ctx context.Context, h Host, name string) error {
	if err := ensureUp(ctx, h, name); err != nil {
		return err
	}
	addrs, err := h.IPv4Addrs(ctx, name)
	if err != nil {
		return err
	}
	for _, a := range addrs {
		if err := h.DelAddr(ctx, name, a); err != nil {
			return fmt.Errorf("remove %s from %s: %w", a, name, err)
		}
		slog.Info("hostnet: removed address from OVS port", "link", name, "cidr", a)
	}
	return nil
}

func ensureRoute(ctx context.Context, h Host, r Route) error {
	routes, err := h.Routes(ctx, r.Dst)
	if err != nil {
		return err
	}
	for _, e := range routes {
		if e.Metric == r.Metric && e.Dev == r.Dev && e.Src == r.Src {
			return nil
		}
	}
	if err := h.ReplaceRoute(ctx, r); err != nil {
		return fmt.Errorf("encap route %s dev %s src %s metric %d: %w", r.Dst, r.Dev, r.Src, r.Metric, err)
	}
	slog.Info("hostnet: set encap route", "dst", r.Dst, "dev", r.Dev, "src", r.Src, "metric", r.Metric)
	return nil
}
