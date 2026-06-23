package host

import (
	"context"
	"fmt"
	"hash/fnv"
	"log/slog"
	"strconv"
)

// imdsReplyNexthop is a synthetic, host-only next-hop for the per-tap reply route.
// It never appears on the wire — the egress OVS flow rewrites the reply's L2 — so it
// only gives the kernel a fixed neigh to resolve instead of ARPing the guest IP.
const imdsReplyNexthop = "169.254.1.1"

// imdsReplyTable returns the per-tap policy-routing table ID for an endpoint, mapped
// into [256, 2^31): clear of the reserved tables (all < 256) and within iproute2's
// positive range. Same entropy as imdsFlowCookie, so collisions here mirror there.
func imdsReplyTable(endpoint string) int {
	h := fnv.New32a()
	_, _ = h.Write([]byte(endpoint))
	return 256 + int(h.Sum32()%(1<<31-256))
}

// InstallTapReplyRouting installs the per-tap reply path: a dedicated routing table
// with one default route out the endpoint, selected by an `ip rule` matching the
// reply's oif. This is the netns-free fix for the overlapping-CIDR main-table
// collision. Idempotent. The endpoint must already exist (see InstallTapDatapath).
func InstallTapReplyRouting(ctx context.Context, r Runner, d IMDSTapDatapath) error {
	if d.Endpoint == "" {
		return fmt.Errorf("InstallTapReplyRouting: Endpoint required")
	}
	if d.GuestMAC == "" {
		return fmt.Errorf("InstallTapReplyRouting: GuestMAC required")
	}
	table := strconv.Itoa(imdsReplyTable(d.Endpoint))

	// One default route out the endpoint. onlink because the synthetic next-hop is
	// off-subnet; a permanent neigh resolves it so the kernel emits without ARPing.
	// lladdr is cosmetic — the egress OVS flow rewrites dl_dst to the guest.
	if _, err := r.Run(ctx, "ip", "route", "replace", "default", "via", imdsReplyNexthop,
		"dev", d.Endpoint, "onlink", "table", table); err != nil {
		return fmt.Errorf("install IMDS reply route (table %s): %w", table, err)
	}
	if _, err := r.Run(ctx, "ip", "neigh", "replace", imdsReplyNexthop, "lladdr", d.GuestMAC,
		"dev", d.Endpoint, "nud", "permanent"); err != nil {
		return fmt.Errorf("install IMDS reply neigh (endpoint %s): %w", d.Endpoint, err)
	}

	// `ip rule add` appends duplicates, so delete any prior rule for this endpoint
	// before adding exactly one. del-by-selector matches our rule uniquely (oif).
	_, _ = r.Run(ctx, "ip", "rule", "del", "oif", d.Endpoint, "lookup", table)
	if _, err := r.Run(ctx, "ip", "rule", "add", "oif", d.Endpoint, "lookup", table); err != nil {
		return fmt.Errorf("install IMDS reply rule (oif %s table %s): %w", d.Endpoint, table, err)
	}
	slog.Info("IMDS tap reply routing installed", "endpoint", d.Endpoint, "table", table)
	return nil
}

// RemoveTapReplyRouting tears down the per-tap reply path. Best-effort: deleting the
// endpoint drops its routes and neigh, but the `ip rule` is keyed by name and would
// leak, so this must run while the endpoint still exists. Idempotent.
func RemoveTapReplyRouting(ctx context.Context, r Runner, d IMDSTapDatapath) error {
	if d.Endpoint == "" {
		return fmt.Errorf("RemoveTapReplyRouting: Endpoint required")
	}
	table := strconv.Itoa(imdsReplyTable(d.Endpoint))
	if _, err := r.Run(ctx, "ip", "rule", "del", "oif", d.Endpoint, "lookup", table); err != nil {
		slog.Warn("Failed to remove IMDS reply rule", "endpoint", d.Endpoint, "table", table, "err", err)
	}
	if _, err := r.Run(ctx, "ip", "route", "flush", "table", table); err != nil {
		slog.Warn("Failed to flush IMDS reply table", "table", table, "err", err)
	}
	if _, err := r.Run(ctx, "ip", "neigh", "del", imdsReplyNexthop, "dev", d.Endpoint); err != nil {
		slog.Debug("IMDS reply neigh already absent", "endpoint", d.Endpoint, "err", err)
	}
	slog.Info("IMDS tap reply routing removed", "endpoint", d.Endpoint)
	return nil
}
