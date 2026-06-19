package host

import (
	"context"
	"fmt"
	"hash/fnv"
	"log/slog"
	"strings"

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

// IMDSEndpointName returns the per-tap endpoint port on IMDSBridge — the
// SO_BINDTODEVICE target the responder binds. "ime-" + 8-char short ENI = 12
// chars, within the 15-char IFNAMSIZ limit.
func IMDSEndpointName(eniID string) string {
	id := strings.TrimPrefix(eniID, "eni-")
	if len(id) > 8 {
		id = id[len(id)-8:]
	}
	return "ime-" + id
}

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

// IMDSTapDatapath parameterises a single tap's IMDS datapath on IMDSBridge. Tap
// is the guest's port on IMDSBridge; Endpoint is its internal-port responder
// target. The egress flow rewrites L2 so the reply looks like it came from the
// guest's gateway (the guest reaches .254 off-link via GatewayMAC).
type IMDSTapDatapath struct {
	Tap         string
	Endpoint    string
	EndpointMAC string
	GuestMAC    string
	GatewayMAC  string
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

// InstallTapDatapath realises a tap's IMDS datapath on IMDSBridge: a per-tap
// internal endpoint owning the captured addresses, the ingress demux flows that
// rewrite the gateway dst MAC to the endpoint, and the egress flow back to the
// tap. Idempotent. Reply routing (ip rule/route/neigh) is installed separately.
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
	if _, err := r.Run(ctx, "ovs-vsctl", "--if-exists", "del-port", IMDSBridge, d.Endpoint); err != nil {
		return fmt.Errorf("delete IMDS endpoint %s: %w", d.Endpoint, err)
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
	for _, addr := range imdsCaptureAddrs {
		out, err := r.Run(ctx, "ip", "addr", "add", addr+"/32", "dev", d.Endpoint)
		if err != nil && !strings.Contains(string(out), "File exists") {
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

// installIMDSTapFlows installs the per-tap ingress demux and egress flows under
// the tap's cookie, clearing any stale flows for the endpoint first.
func installIMDSTapFlows(ctx context.Context, r Runner, d IMDSTapDatapath) error {
	cookie := imdsFlowCookie(d.Endpoint)
	if err := clearIMDSFlowsByCookie(ctx, r, cookie); err != nil {
		return err
	}
	// Ingress: guest -> endpoint. The guest addresses the frame to its gateway
	// MAC (.254/.253 are off-link, routed via the default gateway), so rewrite
	// the dst MAC to the endpoint or the kernel drops it as OTHERHOST.
	for _, addr := range imdsCaptureAddrs {
		spec := fmt.Sprintf("table=0,priority=200,in_port=%s,ip,nw_dst=%s,actions=mod_dl_dst:%s,output:%s",
			d.Tap, addr, d.EndpointMAC, d.Endpoint)
		if err := installIMDSFlow(ctx, r, cookie, spec); err != nil {
			return err
		}
	}
	// Egress: endpoint -> guest, rewriting L2 so the reply looks like it came
	// from the gateway. Steered by flow, not the host routing table.
	egress := fmt.Sprintf("table=0,priority=200,in_port=%s,ip,actions=mod_dl_src:%s,mod_dl_dst:%s,output:%s",
		d.Endpoint, d.GatewayMAC, d.GuestMAC, d.Tap)
	return installIMDSFlow(ctx, r, cookie, egress)
}
