package host

import (
	"context"
	"fmt"
	"strings"
)

// FlushNeigh removes the kernel ARP entry for ip on dev, forcing re-resolution.
// Same-chassis EIP rebind skips OVN's automatic GARP, so flushing is required.
// Best-effort: callers should treat errors as warnings.
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

// ReplaceNeigh installs the kernel ARP entry for ip→mac on dev (NUD_REACHABLE).
// OVN emits no GARP on same-chassis EIP rebind, so programming the MAC directly
// avoids an ARP round-trip. Best-effort: callers treat errors as warnings.
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
