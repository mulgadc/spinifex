package host

import (
	"context"
	"errors"

	"github.com/mulgadc/spinifex/spinifex/utils"
	"github.com/mulgadc/spinifex/spinifex/vm"
)

// AttachIMDSDatapath realises the per-tap IMDS datapath for a primary-ENI tap:
// the br-imds<->br-int patch (carrying the guest's non-IMDS traffic to OVN), the
// br-imds endpoint, ingress demux + egress flows, and reply routing. Wired into
// the tap lifecycle at launch, after SetupTap has placed the primary tap on
// br-imds (before QEMU starts the guest). Idempotent. Serving is handled
// separately by the IMDS responder.
func (p *OVSPlumber) AttachIMDSDatapath(eniID, mac, subnetID string) error {
	return installIMDSDatapath(context.Background(), NewExecRunner(), imdsDatapathSpec(eniID, mac, subnetID))
}

// DetachIMDSDatapath tears down the per-tap IMDS datapath for a primary-ENI tap:
// reply routing, the demux/egress/forward flows, the br-imds<->br-int patch, and
// the endpoint — the inverse of AttachIMDSDatapath. The shared br-imds bridge is
// left in place. Wired into the tap lifecycle at terminate. Idempotent (safe for
// an ENI whose datapath was never installed).
func (p *OVSPlumber) DetachIMDSDatapath(eniID string) error {
	return removeIMDSDatapath(context.Background(), NewExecRunner(), imdsDetachSpec(eniID))
}

// EnsureIMDSDatapathBridge idempotently creates the dedicated IMDS bridge. The
// daemon calls it before SetupTap places a primary tap on br-imds, since OVS
// cannot add a port to a bridge that does not exist yet.
func (p *OVSPlumber) EnsureIMDSDatapathBridge() error {
	return EnsureIMDSBridge(context.Background(), NewExecRunner())
}

// imdsDatapathSpec derives the per-tap datapath parameters from the ENI's
// identity, guest MAC, and subnet. GatewayMAC matches the subnet's OVN router
// port MAC (utils.HashMAC(subnetID)) — the gateway the guest addresses .254/.253
// frames to, which the ingress demux rewrites to the endpoint and the egress
// flow restores as the reply source. PatchInt carries the OVN iface-id binding
// (vm.OVSIfaceID(eniID)) so ovn-controller binds the guest LSP to the patch.
func imdsDatapathSpec(eniID, mac, subnetID string) IMDSTapDatapath {
	return IMDSTapDatapath{
		Tap:         vm.TapDeviceName(eniID),
		Endpoint:    IMDSEndpointName(eniID),
		EndpointMAC: IMDSEndpointMAC(eniID),
		GuestMAC:    mac,
		GatewayMAC:  utils.HashMAC(subnetID),
		PatchIMDS:   IMDSPatchPort(eniID),
		PatchInt:    IMDSIntPatchPort(eniID),
		IfaceID:     vm.OVSIfaceID(eniID),
	}
}

// imdsDetachSpec derives the identifiers teardown keys off: the endpoint (its
// flow cookie + reply table) and the patch pair. RemoveTapDatapath and
// RemoveTapReplyRouting need no guest/gateway MAC, so those are omitted.
func imdsDetachSpec(eniID string) IMDSTapDatapath {
	return IMDSTapDatapath{
		Endpoint:  IMDSEndpointName(eniID),
		PatchIMDS: IMDSPatchPort(eniID),
		PatchInt:  IMDSIntPatchPort(eniID),
	}
}

// installIMDSDatapath ensures br-imds exists, then realises the per-tap datapath:
// the br-imds<->br-int patch (so the guest's non-IMDS traffic still reaches OVN),
// the endpoint + demux/egress flows, and reply routing. Ordering matters: the
// bridge holds the ports, and the endpoint must exist before reply routing
// references it. The primary tap is already on br-imds (SetupTap) before this runs.
func installIMDSDatapath(ctx context.Context, r Runner, d IMDSTapDatapath) error {
	if err := EnsureIMDSBridge(ctx, r); err != nil {
		return err
	}
	if err := installTapPatch(ctx, r, d); err != nil {
		return err
	}
	if err := InstallTapDatapath(ctx, r, d); err != nil {
		return err
	}
	return InstallTapReplyRouting(ctx, r, d)
}

// removeIMDSDatapath reverses installIMDSDatapath. Reply routing goes first: its
// ip rule is keyed by the endpoint name and must be dropped while the endpoint
// exists, before RemoveTapDatapath deletes the endpoint port. Both run regardless
// of the other's outcome so a partial install still tears fully down. The shared
// br-imds bridge is never removed here.
func removeIMDSDatapath(ctx context.Context, r Runner, d IMDSTapDatapath) error {
	replyErr := RemoveTapReplyRouting(ctx, r, d)
	dpErr := RemoveTapDatapath(ctx, r, d)
	return errors.Join(replyErr, dpErr)
}
