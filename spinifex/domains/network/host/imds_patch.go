package host

import (
	"context"
	"fmt"
	"log/slog"
)

// installTapPatch creates the IMDSBridge<->br-int patch pair for a primary tap and
// the per-tap forward flows bridging non-IMDS traffic between the tap and the patch
// — everything that is not .254/.253 must still reach OVN. The br-int patch end
// carries the OVN iface-id + attached-mac so ovn-controller binds the guest LSP to
// it exactly as it bound the tap. Forward flows sit below the demux. Idempotent.
func installTapPatch(ctx context.Context, r Runner, d IMDSTapDatapath) error {
	if err := d.validatePatch(); err != nil {
		return err
	}
	// IMDSBridge end of the patch.
	if _, err := r.Run(ctx, "ovs-vsctl", "--may-exist", "add-port", IMDSBridge, d.PatchIMDS,
		"--", "set", "Interface", d.PatchIMDS, "type=patch", "options:peer="+d.PatchInt); err != nil {
		return fmt.Errorf("create IMDS patch %s on %s: %w", d.PatchIMDS, IMDSBridge, err)
	}
	// br-int end: carries the OVN binding so ovn-controller binds the guest LSP
	// to it by iface-id, exactly as it bound the tap.
	if _, err := r.Run(ctx, "ovs-vsctl", "--may-exist", "add-port", "br-int", d.PatchInt,
		"--", "set", "Interface", d.PatchInt, "type=patch", "options:peer="+d.PatchIMDS,
		"external_ids:iface-id="+d.IfaceID, "external_ids:attached-mac="+d.GuestMAC); err != nil {
		return fmt.Errorf("create IMDS patch %s on br-int: %w", d.PatchInt, err)
	}

	// Forward flows: non-IMDS traffic tap<->patch. IMDSBridge is fail-mode=secure
	// (no NORMAL flooding), so the bridging is explicit. Priority below the demux
	// flows leaves .254/.253 to be intercepted.
	cookie := imdsFlowCookie(d.Endpoint)
	out := fmt.Sprintf("table=0,priority=%d,in_port=%s,actions=output:%s", imdsForwardPriority, d.Tap, d.PatchIMDS)
	if err := installIMDSFlow(ctx, r, cookie, out); err != nil {
		return err
	}
	in := fmt.Sprintf("table=0,priority=%d,in_port=%s,actions=output:%s", imdsForwardPriority, d.PatchIMDS, d.Tap)
	if err := installIMDSFlow(ctx, r, cookie, in); err != nil {
		return err
	}
	slog.Info("IMDS tap patch installed", "tap", d.Tap, "patch_imds", d.PatchIMDS, "patch_int", d.PatchInt)
	return nil
}
