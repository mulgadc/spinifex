package host

import (
	"context"
	"errors"
	"fmt"
	"net/netip"

	networkids "github.com/mulgadc/spinifex/spinifex/domains/network/identifiers"
	"github.com/mulgadc/spinifex/spinifex/runtime/compute/vm"
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

// IMDSDatapathStates reports what survives of each primary ENI's datapath, so
// the daemon can re-install one that an ovs-vswitchd restart or a kernel-module
// reload took down.
func (p *OVSPlumber) IMDSDatapathStates(ctx context.Context, eniIDs []string) (map[string]vm.IMDSDatapathState, error) {
	return IMDSDatapathStates(ctx, NewExecRunner(), eniIDs)
}

// RepairIMDSDatapath re-installs a running guest's datapath. It returns
// vm.ErrIMDSTapDetached, changing nothing, once the tap has left br-imds.
func (p *OVSPlumber) RepairIMDSDatapath(ctx context.Context, eniID, mac, subnetID string) error {
	return repairIMDSDatapath(ctx, NewExecRunner(), imdsDatapathSpec(eniID, mac, subnetID))
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
		GatewayMAC:  networkids.HashMAC(subnetID),
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

// installIMDSDatapath realises the per-tap datapath on an existing br-imds:
// endpoint, patch, reply routing, all flows in one bundle, then the sysctls.
// Flow and reply-routing errors wrap ErrIMDSServingDegraded; others are bare.
func installIMDSDatapath(ctx context.Context, r Runner, d IMDSTapDatapath) error {
	if err := d.validate(); err != nil {
		return err
	}
	// The INPUT accept and the remap DNAT are wildcarded over every endpoint and
	// installed once by vpcd at start. The daemon has no iptables grant.
	if err := ensureIMDSEndpoint(ctx, r, d); err != nil {
		return err
	}
	// ListIMDSTaps advertises the tap off the patch and vpcd binds the endpoint,
	// so the endpoint must exist whenever the patch does.
	flows, err := installTapPatch(ctx, r, d)
	if err != nil {
		return err
	}
	tapFlows, err := imdsTapFlows(d)
	if err != nil {
		return err
	}
	// The launch caller aborts on a serving error too: guest bootstrap needs IMDS.
	if err := InstallTapReplyRouting(ctx, r, d); err != nil {
		return fmt.Errorf("%w: %w", vm.ErrIMDSServingDegraded, err)
	}
	if err := installIMDSFlowBundle(ctx, r, imdsFlowCookie(d.Endpoint), append(flows, tapFlows...)); err != nil {
		return fmt.Errorf("%w: %w", vm.ErrIMDSServingDegraded, err)
	}
	// Last: accept_local marks the datapath complete for the drift check.
	return setEndpointSysctls(ctx, r, d.Endpoint)
}

// removeIMDSDatapath reverses installIMDSDatapath. Reply routing goes first: its
// ip rule must be dropped while the endpoint exists, before RemoveTapDatapath
// deletes it. Both run regardless of the other's outcome. br-imds is left in place.
func removeIMDSDatapath(ctx context.Context, r Runner, d IMDSTapDatapath) error {
	replyErr := RemoveTapReplyRouting(ctx, r, d)
	dpErr := RemoveTapDatapath(ctx, r, d)
	return errors.Join(replyErr, dpErr)
}

// EndpointDemuxStates reports the gateway-endpoint destinations each primary
// ENI's tap currently demuxes to the proxy, from one bridge dump.
func (p *OVSPlumber) EndpointDemuxStates(ctx context.Context, eniIDs []string) (vm.EndpointDemuxStates, error) {
	states, unowned, err := EndpointDemuxStates(ctx, NewExecRunner(), eniIDs)
	if err != nil {
		return vm.EndpointDemuxStates{}, err
	}
	out := vm.EndpointDemuxStates{ByENI: make(map[string][]netip.AddrPort, len(states)), Unowned: unowned}
	for eni, entries := range states {
		aps := make([]netip.AddrPort, len(entries))
		for i, e := range entries {
			aps[i] = netip.AddrPortFrom(e.Addr, e.Port)
		}
		out.ByENI[eni] = aps
	}
	return out, nil
}

// RemoveEndpointDemuxCookie removes the demux flows under a cookie no running
// guest owns.
func (p *OVSPlumber) RemoveEndpointDemuxCookie(ctx context.Context, cookie uint64) error {
	return RemoveEndpointDemuxCookie(ctx, NewExecRunner(), cookie)
}

// EnsureEndpointDemux makes eniID's tap demux exactly entries to the proxy;
// an empty set removes its demux.
func (p *OVSPlumber) EnsureEndpointDemux(ctx context.Context, eniID string, entries []netip.AddrPort) error {
	es := make([]EndpointEntry, len(entries))
	for i, ap := range entries {
		es[i] = EndpointEntry{Addr: ap.Addr(), Port: ap.Port()}
	}
	return EnsureEndpointDemux(ctx, NewExecRunner(), eniID, es)
}
