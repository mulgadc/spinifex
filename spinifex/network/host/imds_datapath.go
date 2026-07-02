package host

import (
	"context"
	"errors"
	"fmt"
	"hash/fnv"
	"log/slog"

	"github.com/mulgadc/spinifex/spinifex/utils"
)

// IMDS / VPC-DNS service addresses captured at every tap. .254 is IMDS, served
// by this datapath; .253 is the VPC-DNS link-local alias, captured here and
// reserved for the Northstar resolver (same per-tap capture, demuxed by dst).
const (
	imdsMetaAddr = "169.254.169.254"
	imdsDNSAddr  = "169.254.169.253"
)

// imdsCaptureAddrs are the link-local addresses demuxed to the per-tap endpoint.
// Kept in one place so the endpoint addresses and the ingress flows stay in sync.
var imdsCaptureAddrs = []string{imdsMetaAddr, imdsDNSAddr}

// Per-tap OpenFlow priorities on IMDSBridge. Demux (.254/.253 interception) and
// egress sit above the forward flows so IMDS traffic is captured and everything
// else is bridged tap<->patch to br-int.
const (
	imdsDemuxPriority   = 200
	imdsForwardPriority = 100
)

// shortENIID returns a stable 8-hex-char tag hashed (FNV-32a) from the full ENI.
// Per-tap port names key off it to stay within the 15-char IFNAMSIZ limit; hashing
// the whole ENI rather than truncating keeps ENIs sharing a suffix distinct.
func shortENIID(eniID string) string {
	h := fnv.New32a()
	_, _ = h.Write([]byte(eniID))
	return fmt.Sprintf("%08x", h.Sum32())
}

// IMDSEndpointName returns the per-tap endpoint port on IMDSBridge — the
// SO_BINDTODEVICE target the responder binds. "ime-" + 8-char short ENI = 12
// chars, within the 15-char IFNAMSIZ limit.
func IMDSEndpointName(eniID string) string { return "ime-" + shortENIID(eniID) }

// IMDSPatchPort returns the IMDSBridge end of the per-tap patch to br-int.
// "imp-" + 8-char short ENI = 12 chars.
func IMDSPatchPort(eniID string) string { return "imp-" + shortENIID(eniID) }

// imdsIntPatchPrefix tags the br-int end of every per-tap patch. vpcd's
// reconcile-from-taps keys off it to find the local IMDS taps (see ListIMDSTaps).
const imdsIntPatchPrefix = "imi-"

// IMDSIntPatchPort returns the br-int end of the per-tap patch. It carries the
// OVN iface-id binding so ovn-controller binds the guest LSP to it. "imi-" +
// 8-char short ENI = 12 chars.
func IMDSIntPatchPort(eniID string) string { return imdsIntPatchPrefix + shortENIID(eniID) }

// IMDSEndpointMAC returns the deterministic MAC for an ENI's endpoint. The
// endpoint owns this MAC so the ingress demux can rewrite the guest's gateway
// dst MAC to it (the kernel drops the frame as OTHERHOST otherwise).
func IMDSEndpointMAC(eniID string) string {
	return utils.HashMAC("imds-ep:" + eniID)
}

// imdsFlowCookie returns the per-tap OpenFlow cookie tagging endpoint's flows.
// The imdsCookiePrefix group tag marks IMDS flows; the per-endpoint suffix lets
// a tap teardown delete exactly its own flows even after the tap port is gone.
func imdsFlowCookie(endpoint string) string {
	h := fnv.New32a()
	_, _ = h.Write([]byte(endpoint))
	return fmt.Sprintf("%s%08x", imdsCookiePrefix, h.Sum32())
}

// IMDSTapDatapath parameterises a single tap's IMDS datapath on IMDSBridge. Tap is
// the guest's port; Endpoint is its internal-port responder target. The egress flow
// rewrites L2 so the reply looks like it came from the guest's gateway (GatewayMAC).
type IMDSTapDatapath struct {
	Tap         string
	Endpoint    string
	EndpointMAC string
	GuestMAC    string
	GatewayMAC  string

	// Patch ports bridging non-IMDS traffic between the primary tap and br-int.
	// PatchIMDS is the IMDSBridge end; PatchInt is the br-int end and carries the OVN
	// binding (IfaceID + GuestMAC) so ovn-controller binds the guest LSP to it.
	PatchIMDS string
	PatchInt  string
	IfaceID   string
}

func (d IMDSTapDatapath) validate() error {
	switch {
	case d.Tap == "":
		return fmt.Errorf("IMDSTapDatapath: Tap required")
	case d.Endpoint == "":
		return fmt.Errorf("IMDSTapDatapath: Endpoint required")
	case d.EndpointMAC == "":
		return fmt.Errorf("IMDSTapDatapath: EndpointMAC required")
	case d.GuestMAC == "":
		return fmt.Errorf("IMDSTapDatapath: GuestMAC required")
	case d.GatewayMAC == "":
		return fmt.Errorf("IMDSTapDatapath: GatewayMAC required")
	}
	return nil
}

// validatePatch checks the fields installTapPatch needs (independent of the
// demux/egress fields validate covers, since the patch needs no GatewayMAC).
func (d IMDSTapDatapath) validatePatch() error {
	switch {
	case d.Tap == "":
		return fmt.Errorf("IMDSTapDatapath: Tap required")
	case d.PatchIMDS == "":
		return fmt.Errorf("IMDSTapDatapath: PatchIMDS required")
	case d.PatchInt == "":
		return fmt.Errorf("IMDSTapDatapath: PatchInt required")
	case d.IfaceID == "":
		return fmt.Errorf("IMDSTapDatapath: IfaceID required")
	case d.GuestMAC == "":
		return fmt.Errorf("IMDSTapDatapath: GuestMAC required")
	}
	return nil
}

// InstallTapDatapath realises a tap's IMDS datapath on IMDSBridge: the per-tap
// endpoint owning the captured addresses, the ingress demux flows, and the egress
// flow back to the tap. Idempotent. Reply routing is installed separately.
func InstallTapDatapath(ctx context.Context, r Runner, d IMDSTapDatapath) error {
	if err := d.validate(); err != nil {
		return err
	}
	if err := ensureIMDSEndpoint(ctx, r, d); err != nil {
		return err
	}
	if err := installIMDSTapFlows(ctx, r, d); err != nil {
		return err
	}
	slog.Info("IMDS tap datapath installed", "tap", d.Tap, "endpoint", d.Endpoint)
	return nil
}

// RemoveTapDatapath tears down a tap's IMDS datapath: its flows (by per-tap
// cookie, robust to the tap port already being gone) and its endpoint port,
// which takes the endpoint's addresses and per-device sysctls with it. Idempotent.
func RemoveTapDatapath(ctx context.Context, r Runner, d IMDSTapDatapath) error {
	if d.Endpoint == "" {
		return fmt.Errorf("RemoveTapDatapath: Endpoint required")
	}
	if err := clearIMDSFlowsByCookie(ctx, r, imdsFlowCookie(d.Endpoint)); err != nil {
		slog.Warn("Failed to clear IMDS tap flows", "endpoint", d.Endpoint, "err", err)
	}
	// Every port is deleted regardless of an earlier failure, or teardown leaks the
	// endpoint (and its captured .254/.253 addresses) on br-imds. The IMDSBridge patch
	// end is best-effort; the br-int end and endpoint surface their errors.
	if d.PatchIMDS != "" {
		if _, err := r.Run(ctx, "ovs-vsctl", "--if-exists", "del-port", IMDSBridge, d.PatchIMDS); err != nil {
			slog.Warn("Failed to delete IMDS patch (br-imds end)", "port", d.PatchIMDS, "err", err)
		}
	}
	var errs []error
	if d.PatchInt != "" {
		if _, err := r.Run(ctx, "ovs-vsctl", "--if-exists", "del-port", "br-int", d.PatchInt); err != nil {
			errs = append(errs, fmt.Errorf("delete IMDS patch %s on br-int: %w", d.PatchInt, err))
		}
	}
	if _, err := r.Run(ctx, "ovs-vsctl", "--if-exists", "del-port", IMDSBridge, d.Endpoint); err != nil {
		errs = append(errs, fmt.Errorf("delete IMDS endpoint %s: %w", d.Endpoint, err))
	}
	if len(errs) > 0 {
		return errors.Join(errs...)
	}
	slog.Info("IMDS tap datapath removed", "endpoint", d.Endpoint)
	return nil
}

// ensureIMDSEndpoint creates the per-tap internal port, sets its MAC, brings it
// up, owns the captured addresses, and sets the per-device sysctls the
// asymmetric host path needs (guest src IP arrives with no forward route).
func ensureIMDSEndpoint(ctx context.Context, r Runner, d IMDSTapDatapath) error {
	if _, err := r.Run(ctx, "ovs-vsctl", "--may-exist", "add-port", IMDSBridge, d.Endpoint,
		"--", "set", "Interface", d.Endpoint, "type=internal"); err != nil {
		return fmt.Errorf("create IMDS endpoint %s: %w", d.Endpoint, err)
	}
	if _, err := r.Run(ctx, "ip", "link", "set", d.Endpoint, "address", d.EndpointMAC); err != nil {
		return fmt.Errorf("set IMDS endpoint %s MAC: %w", d.Endpoint, err)
	}
	if _, err := r.Run(ctx, "ip", "link", "set", d.Endpoint, "up"); err != nil {
		return fmt.Errorf("bring up IMDS endpoint %s: %w", d.Endpoint, err)
	}
	// `replace` is idempotent — adds the /32 if absent, no-op if the endpoint
	// already owns it (a recovery/stop re-attach reuses a surviving endpoint).
	// `add` errored on the duplicate, and its kernel message varies by version.
	for _, addr := range imdsCaptureAddrs {
		if _, err := r.Run(ctx, "ip", "addr", "replace", addr+"/32", "dev", d.Endpoint); err != nil {
			return fmt.Errorf("add %s to IMDS endpoint %s: %w", addr, d.Endpoint, err)
		}
	}
	if err := setEndpointSysctl(ctx, r, d.Endpoint, "rp_filter", "0"); err != nil {
		return err
	}
	return setEndpointSysctl(ctx, r, d.Endpoint, "accept_local", "1")
}

func setEndpointSysctl(ctx context.Context, r Runner, endpoint, suffix, val string) error {
	key := "net.ipv4.conf." + endpoint + "." + suffix
	if _, err := r.Run(ctx, "sysctl", "-qw", key+"="+val); err != nil {
		return fmt.Errorf("set %s=%s: %w", key, val, err)
	}
	return nil
}

// installIMDSTapFlows installs the per-tap ingress demux and egress flows under the
// tap's cookie. The caller clears stale flows before any installer runs; clearing
// here would wipe the forward flows the patch installer added under the same cookie.
func installIMDSTapFlows(ctx context.Context, r Runner, d IMDSTapDatapath) error {
	cookie := imdsFlowCookie(d.Endpoint)
	// Ingress: guest -> endpoint. The guest addresses the frame to its gateway
	// MAC (.254/.253 are off-link, routed via the default gateway), so rewrite
	// the dst MAC to the endpoint or the kernel drops it as OTHERHOST.
	for _, addr := range imdsCaptureAddrs {
		spec := fmt.Sprintf("table=0,priority=%d,in_port=%s,ip,nw_dst=%s,actions=mod_dl_dst:%s,output:%s",
			imdsDemuxPriority, d.Tap, addr, d.EndpointMAC, d.Endpoint)
		if err := installIMDSFlow(ctx, r, cookie, spec); err != nil {
			return err
		}
	}
	// Egress: endpoint -> guest, rewriting L2 so the reply looks like it came
	// from the gateway. Steered by flow, not the host routing table.
	egress := fmt.Sprintf("table=0,priority=%d,in_port=%s,ip,actions=mod_dl_src:%s,mod_dl_dst:%s,output:%s",
		imdsDemuxPriority, d.Endpoint, d.GatewayMAC, d.GuestMAC, d.Tap)
	return installIMDSFlow(ctx, r, cookie, egress)
}
