package host

import (
	"context"
	"errors"
	"fmt"

	"github.com/mulgadc/spinifex/spinifex/utils"
	"github.com/mulgadc/spinifex/spinifex/vm"
)

// AttachIMDSDatapath realises the per-tap IMDS datapath for a primary-ENI tap: the
// br-imds<->br-int patch, the endpoint, ingress demux + egress flows, and reply
// routing. Called at launch after SetupTap places the tap on br-imds. Idempotent.
func (p *OVSPlumber) AttachIMDSDatapath(eniID, mac, subnetID string) error {
	return installIMDSDatapath(context.Background(), NewExecRunner(), imdsDatapathSpec(eniID, mac, subnetID))
}

// DetachIMDSDatapath tears down a primary-ENI tap's IMDS datapath — the inverse of
// AttachIMDSDatapath. The shared br-imds bridge is left in place. Called at
// terminate. Idempotent (safe for an ENI whose datapath was never installed).
func (p *OVSPlumber) DetachIMDSDatapath(eniID string) error {
	return removeIMDSDatapath(context.Background(), NewExecRunner(), imdsDetachSpec(eniID))
}

// EnsureIMDSDatapathBridge idempotently creates the dedicated IMDS bridge. The
// daemon calls it before SetupTap places a primary tap on br-imds, since OVS
// cannot add a port to a bridge that does not exist yet.
func (p *OVSPlumber) EnsureIMDSDatapathBridge() error {
	return EnsureIMDSBridge(context.Background(), NewExecRunner())
}

// imdsDatapathSpec derives the per-tap datapath parameters. GatewayMAC matches the
// subnet's OVN router-port MAC (the gateway the guest sends .254/.253 frames to);
// PatchInt carries the OVN iface-id so ovn-controller binds the guest LSP to it.
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
// patch, endpoint + demux/egress flows, and reply routing. Stale flows are cleared
// once here (all installers share the tap's cookie) before any flow is added.
//
// The bridge, cookie clear, and patch are connectivity-critical — until they exist
// the guest has no L2 path to OVN — so their errors return bare. The demux/egress
// flows and reply routing only enable serving, so theirs wrap ErrIMDSServingDegraded.
func installIMDSDatapath(ctx context.Context, r Runner, d IMDSTapDatapath) error {
	if err := EnsureIMDSBridge(ctx, r); err != nil {
		return err
	}
	if err := clearIMDSFlowsByCookie(ctx, r, imdsFlowCookie(d.Endpoint)); err != nil {
		return err
	}
	if err := installTapPatch(ctx, r, d); err != nil {
		return err
	}
	if err := InstallTapDatapath(ctx, r, d); err != nil {
		return fmt.Errorf("%w: %w", vm.ErrIMDSServingDegraded, err)
	}
	if err := InstallTapReplyRouting(ctx, r, d); err != nil {
		return fmt.Errorf("%w: %w", vm.ErrIMDSServingDegraded, err)
	}
	return nil
}

// removeIMDSDatapath reverses installIMDSDatapath. Reply routing goes first: its
// ip rule must be dropped while the endpoint exists, before RemoveTapDatapath
// deletes it. Both run regardless of the other's outcome. br-imds is left in place.
func removeIMDSDatapath(ctx context.Context, r Runner, d IMDSTapDatapath) error {
	replyErr := RemoveTapReplyRouting(ctx, r, d)
	dpErr := RemoveTapDatapath(ctx, r, d)
	return errors.Join(replyErr, dpErr)
}
