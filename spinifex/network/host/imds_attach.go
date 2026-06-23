package host

import (
	"context"

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
