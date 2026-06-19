package host

import (
	"context"

	"github.com/mulgadc/spinifex/spinifex/utils"
	"github.com/mulgadc/spinifex/spinifex/vm"
)

// AttachIMDSDatapath realises the per-tap IMDS datapath for a primary-ENI tap:
// the br-imds endpoint, ingress demux + egress flows, and reply routing. Wired
// into the tap lifecycle at launch (before QEMU starts the guest). Idempotent.
// Serving is handled separately by the IMDS responder.
func (p *OVSPlumber) AttachIMDSDatapath(eniID, mac, subnetID string) error {
	return installIMDSDatapath(context.Background(), NewExecRunner(), imdsDatapathSpec(eniID, mac, subnetID))
}

// imdsDatapathSpec derives the per-tap datapath parameters from the ENI's
// identity, guest MAC, and subnet. GatewayMAC matches the subnet's OVN router
// port MAC (utils.HashMAC(subnetID)) — the gateway the guest addresses .254/.253
// frames to, which the ingress demux rewrites to the endpoint and the egress
// flow restores as the reply source.
func imdsDatapathSpec(eniID, mac, subnetID string) IMDSTapDatapath {
	return IMDSTapDatapath{
		Tap:         vm.TapDeviceName(eniID),
		Endpoint:    IMDSEndpointName(eniID),
		EndpointMAC: IMDSEndpointMAC(eniID),
		GuestMAC:    mac,
		GatewayMAC:  utils.HashMAC(subnetID),
	}
}

// installIMDSDatapath ensures br-imds exists, then realises the tap's datapath
// and reply routing. Ordering matters: the bridge holds the endpoint, the
// endpoint must exist before reply routing references it.
func installIMDSDatapath(ctx context.Context, r Runner, d IMDSTapDatapath) error {
	if err := EnsureIMDSBridge(ctx, r); err != nil {
		return err
	}
	if err := InstallTapDatapath(ctx, r, d); err != nil {
		return err
	}
	return InstallTapReplyRouting(ctx, r, d)
}
