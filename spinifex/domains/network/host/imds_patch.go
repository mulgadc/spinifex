package host

import (
	"context"
	"fmt"
	"log/slog"
)

// installTapPatch creates the IMDSBridge<->br-int patch pair for a primary tap
// and returns the forward flows that bridge non-IMDS traffic between tap and
// patch. The br-int end carries the OVN binding. Idempotent.
func installTapPatch(ctx context.Context, r Runner, d IMDSTapDatapath) ([]string, error) {
	if err := d.validatePatch(); err != nil {
		return nil, err
	}
	// Both ends in one transaction. The br-int end carries the OVN binding so
	// ovn-controller binds the guest LSP to it by iface-id, as it bound the tap.
	if _, err := r.Run(ctx, "ovs-vsctl",
		"--may-exist", "add-port", IMDSBridge, d.PatchIMDS,
		"--", "set", "Interface", d.PatchIMDS, "type=patch", "options:peer="+d.PatchInt,
		"--", "--may-exist", "add-port", "br-int", d.PatchInt,
		"--", "set", "Interface", d.PatchInt, "type=patch", "options:peer="+d.PatchIMDS,
		"external_ids:iface-id="+d.IfaceID, "external_ids:attached-mac="+d.GuestMAC); err != nil {
		return nil, fmt.Errorf("create IMDS patch %s<->%s: %w", d.PatchIMDS, d.PatchInt, err)
	}
	slog.Info("IMDS tap patch installed", "tap", d.Tap, "patch_imds", d.PatchIMDS, "patch_int", d.PatchInt)

	// Forward flows: non-IMDS traffic tap<->patch. IMDSBridge is fail-mode=secure
	// (no NORMAL flooding), so the bridging is explicit. Priority below the demux
	// flows leaves .254/.253 to be intercepted.
	return []string{
		fmt.Sprintf("table=0,priority=%d,in_port=%s,actions=output:%s", imdsForwardPriority, d.Tap, d.PatchIMDS),
		fmt.Sprintf("table=0,priority=%d,in_port=%s,actions=output:%s", imdsForwardPriority, d.PatchIMDS, d.Tap),
	}, nil
}
