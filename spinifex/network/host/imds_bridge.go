package host

import (
	"context"
	"fmt"
	"log/slog"
)

// IMDSBridge is the dedicated OVS bridge that carries the per-tap IMDS / VPC-DNS
// redirect and ARP-responder flows. ovn-controller flushes foreign br-int flows
// wholesale on restart regardless of cookie or table; a bridge absent from
// ovn-bridge-mappings and not named br-int is never touched, so flows here survive.
const IMDSBridge = "br-imds"

// imdsCookiePrefix tags every flow the IMDS datapath installs on IMDSBridge.
// Per-tap cookies extend it with an endpoint suffix so a teardown removes
// exactly one tap's flows (see imdsFlowCookie).
const imdsCookiePrefix = "0xa1d5"

// EnsureIMDSBridge idempotently creates the dedicated IMDS bridge with
// fail-mode=secure (only installed flows forward; no NORMAL L2 flooding) and no
// OVN external_ids, so ovn-controller leaves it alone (see IMDSBridge).
func EnsureIMDSBridge(ctx context.Context, r Runner) error {
	if _, err := r.Run(ctx, "ovs-vsctl", "--may-exist", "add-br", IMDSBridge); err != nil {
		return fmt.Errorf("create %s: %w", IMDSBridge, err)
	}
	if _, err := r.Run(ctx, "ovs-vsctl", "set", "Bridge", IMDSBridge, "fail-mode=secure"); err != nil {
		return fmt.Errorf("set %s fail-mode: %w", IMDSBridge, err)
	}
	if _, err := r.Run(ctx, "ip", "link", "set", IMDSBridge, "up"); err != nil {
		return fmt.Errorf("bring up %s: %w", IMDSBridge, err)
	}
	slog.Debug("IMDS bridge ready", "bridge", IMDSBridge)
	return nil
}

// RemoveIMDSBridge deletes the IMDS bridge and every flow on it. Idempotent:
// --if-exists tolerates an already-absent bridge.
func RemoveIMDSBridge(ctx context.Context, r Runner) error {
	if _, err := r.Run(ctx, "ovs-vsctl", "--if-exists", "del-br", IMDSBridge); err != nil {
		return fmt.Errorf("delete %s: %w", IMDSBridge, err)
	}
	slog.Debug("IMDS bridge removed", "bridge", IMDSBridge)
	return nil
}

// installIMDSFlow adds one ovs-ofctl flow to IMDSBridge under cookie. spec is
// the flow expression without the cookie (match + actions).
func installIMDSFlow(ctx context.Context, r Runner, cookie, spec string) error {
	if _, err := r.Run(ctx, "ovs-ofctl", "add-flow", IMDSBridge, "cookie="+cookie+","+spec); err != nil {
		return fmt.Errorf("install IMDS flow %q: %w", spec, err)
	}
	return nil
}

// clearIMDSFlowsByCookie removes every flow tagged with cookie from IMDSBridge,
// leaving any other flow untouched. OVS does not purge a flow when its in_port
// is deleted, so a per-tap cookie is how a tap teardown drops its own flows.
func clearIMDSFlowsByCookie(ctx context.Context, r Runner, cookie string) error {
	if _, err := r.Run(ctx, "ovs-ofctl", "del-flows", IMDSBridge, "cookie="+cookie+"/-1"); err != nil {
		return fmt.Errorf("clear IMDS flows (cookie %s): %w", cookie, err)
	}
	return nil
}
