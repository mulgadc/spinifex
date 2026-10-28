package host

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
)

const (
	mssClampTable      = "spinifex_mss"
	mssClampV4Overhead = 40
	mssClampV6Overhead = 60
)

// mssClampRuleset returns the nft batch that replaces our MSS clamp tables
// for mode, or "" when the mode has no host hop to clamp on. The whole batch
// is one transaction, and the other family's table is dropped in it so a
// mode change cannot leave a stale clamp behind.
func mssClampRuleset(mode UplinkMode, gatewayMTU int) string {
	v4 := gatewayMTU - mssClampV4Overhead
	v6 := gatewayMTU - mssClampV6Overhead
	var family, stale, port, v4match, v6match string
	switch mode {
	case UplinkModeVeth:
		// The bridge family drops `ip protocol tcp` from a rule on list, so the v4 and
		// v6 rules would both match every packet; `ether type` keeps them apart.
		family, stale, port = "bridge", "inet", VethLinuxEnd
		v4match, v6match = "ether type ip", "ether type ip6"
	case UplinkModeRouted:
		family, stale, port = "inet", "bridge", NATTransitHostEnd
		v4match, v6match = "meta nfproto ipv4", "meta nfproto ipv6"
	default:
		return ""
	}
	var b strings.Builder
	fmt.Fprintf(&b, "add table %s %s\ndelete table %s %s\n", stale, mssClampTable, stale, mssClampTable)
	fmt.Fprintf(&b, "add table %s %s\ndelete table %s %s\n", family, mssClampTable, family, mssClampTable)
	fmt.Fprintf(&b, "table %s %s {\n\tchain clamp {\n\t\ttype filter hook forward priority -150; policy accept;\n", family, mssClampTable)
	for _, rule := range []struct {
		match string
		mss   int
	}{{v4match, v4}, {v6match, v6}} {
		for _, dir := range []string{"iifname", "oifname"} {
			fmt.Fprintf(&b, "\t\t%s %q %s tcp flags & (syn | rst) == syn tcp option maxseg size > %d tcp option maxseg size set %d\n",
				dir, port, rule.match, rule.mss, rule.mss)
		}
	}
	b.WriteString("\t}\n}\n")
	return b.String()
}

// mssClampTeardown drops both families' tables; add-then-delete makes an
// absent table a no-op instead of an error.
const mssClampTeardown = "add table bridge " + mssClampTable + "\ndelete table bridge " + mssClampTable + "\n" +
	"add table inet " + mssClampTable + "\ndelete table inet " + mssClampTable + "\n"

// EnsureMSSClamp clamps the TCP MSS of SYN and SYN-ACK segments crossing the
// host hop of the WAN uplink to gatewayMTU minus the IP and TCP headers, so
// peers that never receive our ICMP still send segments that fit. Physical
// uplinks have no host hop and only clear a table left by an earlier mode.
//
// The batch goes to nft as one argument rather than a temp file: nft parses
// the command line as a single transaction, and nothing is left on disk.
func EnsureMSSClamp(ctx context.Context, r Runner, mode UplinkMode, gatewayMTU int) error {
	if mode == UplinkModePhysical {
		if err := RemoveMSSClamp(ctx, r); err != nil {
			slog.Warn("host: clear stale MSS clamp failed", "mode", mode, "err", err)
		}
		return nil
	}
	rules := mssClampRuleset(mode, gatewayMTU)
	if rules == "" || gatewayMTU <= 0 {
		return nil
	}
	if out, err := r.Run(ctx, "nft", rules); err != nil {
		return fmt.Errorf("install MSS clamp (%s mode): %s: %w", mode, strings.TrimSpace(string(out)), err)
	}
	slog.Info("host: installed MSS clamp", "mode", mode, "mss_v4", gatewayMTU-mssClampV4Overhead, "mss_v6", gatewayMTU-mssClampV6Overhead)
	return nil
}

// RemoveMSSClamp deletes our MSS clamp tables; absent tables are not an error.
func RemoveMSSClamp(ctx context.Context, r Runner) error {
	if out, err := r.Run(ctx, "nft", mssClampTeardown); err != nil {
		return fmt.Errorf("remove MSS clamp: %s: %w", strings.TrimSpace(string(out)), err)
	}
	return nil
}
