package host

import (
	"context"
	"fmt"
	"strings"
)

// FlushNeigh removes the kernel neighbour (ARP) entry for ip on dev so the next
// packet to ip re-resolves L2. Wraps `ip neigh flush to <ip> dev <dev>`, which
// is idempotent — flushing zero entries still succeeds.
//
// inject-garp-independent ARP refresh for EIP recycle: ovn-controller emits its
// automatic GARP only on LSP binding-chassis migration, and the explicit
// `ovn-appctl inject-garp` refresh (see InjectGARP) is unavailable on OVN builds
// without that appctl. Without either, a same-chassis external_ip rebind leaves
// the host neighbour cache pointed at the prior owner's MAC until the kernel ARP
// timeout (60-300s). Flushing the entry on EIP attach and detach forces a fresh
// resolution against the current owner.
//
// L0 method (ADR-0006 S2) — only network/host/ may shell out to host tools.
// Best-effort: callers must treat errors as warnings, not failures.
func FlushNeigh(ctx context.Context, runner Runner, dev, ip string) error {
	if dev == "" || ip == "" {
		return fmt.Errorf("host.FlushNeigh: dev and ip required")
	}
	if runner == nil {
		runner = NewExecRunner()
	}
	out, err := runner.Run(ctx, "ip", "neigh", "flush", "to", ip, "dev", dev)
	if err != nil {
		return fmt.Errorf("ip neigh flush to %s dev %s: %s: %w", ip, dev, strings.TrimSpace(string(out)), err)
	}
	return nil
}

// ReplaceNeigh installs (or overwrites) the kernel neighbour entry mapping ip to
// mac on dev in NUD_REACHABLE state. Wraps `ip neigh replace <ip> lladdr <mac>
// dev <dev> nud reachable`.
//
// This is the deterministic counterpart to FlushNeigh for EIP attach: when an
// external IP is recycled onto a new owner, OVN advertises the new MAC only via
// a GARP it emits on LSP binding-chassis migration — a same-chassis rebind
// emits none, and `ovn-appctl inject-garp` is absent on older OVN builds, so no
// node answers the host's re-ARP. Flushing then waits for an ARP reply that
// never comes; programming the known external_mac directly skips the round-trip,
// and inbound traffic refreshes the entry from there on.
//
// L0 method (ADR-0006 S2) — only network/host/ may shell out to host tools.
// Best-effort: callers must treat errors as warnings, not failures.
func ReplaceNeigh(ctx context.Context, runner Runner, dev, ip, mac string) error {
	if dev == "" || ip == "" || mac == "" {
		return fmt.Errorf("host.ReplaceNeigh: dev, ip and mac required")
	}
	if runner == nil {
		runner = NewExecRunner()
	}
	out, err := runner.Run(ctx, "ip", "neigh", "replace", ip, "lladdr", mac, "dev", dev, "nud", "reachable")
	if err != nil {
		return fmt.Errorf("ip neigh replace %s lladdr %s dev %s: %s: %w", ip, mac, dev, strings.TrimSpace(string(out)), err)
	}
	return nil
}
