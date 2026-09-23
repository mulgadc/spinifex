package host

import (
	"context"
	"fmt"
	"log/slog"
)

// IMDSBridge is the dedicated OVS bridge carrying the per-tap IMDS / VPC-DNS flows.
// ovn-controller flushes foreign br-int flows wholesale on restart; a bridge absent
// from ovn-bridge-mappings and not named br-int is never touched, so flows survive.
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

// imdsInputComment tags the INPUT accept so an operator can find it and the
// uninstaller can remove it.
const imdsInputComment = "spinifex-imds"

// imdsInputSpec accepts guest traffic arriving on any IMDS endpoint. The "+"
// is an iptables interface wildcard, so one rule covers every ime- device.
var imdsInputSpec = []string{
	"-i", IMDSEndpointPrefix + "+",
	"-m", "comment", "--comment", imdsInputComment, "-j", "ACCEPT",
}

// EnsureIMDSInputRule opens INPUT for the IMDS endpoints. The responder binds
// .254/.253 on each ime- device, so guest requests are locally delivered and
// meet INPUT — which on a cloud image is default-deny. Oracle's Ubuntu image
// ships a catch-all REJECT, so without this every guest loses IMDS and VPC DNS,
// and cloud-init spends 490s timing out before giving up.
//
// Inserted at the head for the same reason, and probed with -C first: no
// earlier build appended this rule, so a present one is always one of ours in
// the right place, and re-inserting per launch would blip IMDS for guests
// already running.
func EnsureIMDSInputRule(ctx context.Context, r Runner) error {
	if _, err := r.Run(ctx, "iptables", natRuleArgs("-C", "filter", "INPUT", imdsInputSpec)...); err == nil {
		return nil
	}
	if out, err := r.Run(ctx, "iptables", natInsertArgs("filter", "INPUT", imdsInputSpec)...); err != nil {
		return fmt.Errorf("install IMDS input rule: %s: %w", string(out), err)
	}
	slog.Info("host: installed IMDS input rule", "iface", IMDSEndpointPrefix+"+")
	return nil
}

// RemoveIMDSInputRule drops the INPUT accept; a missing rule is not an error.
func RemoveIMDSInputRule(ctx context.Context, r Runner) {
	if _, err := r.Run(ctx, "iptables", natRuleArgs("-D", "filter", "INPUT", imdsInputSpec)...); err != nil {
		slog.Debug("host: IMDS input rule not present on delete", "err", err)
	}
}

// imdsRemapComment tags the DNAT rules so an operator can find them and the
// uninstaller can remove them.
const imdsRemapComment = "spinifex-imds-remap"

// imdsRemapSpecs are the PREROUTING DNATs that let a host serve IMDS and VPC
// DNS on addresses of its own while guests keep addressing the standard pair.
// Scoped to the ime- endpoints by interface wildcard, so nothing the host
// originates is touched — which is the entire point, since the host's own
// 169.254.169.254 is the cloud's metadata service.
//
// conntrack reverses the translation on the reply, so the guest sees the answer
// sourced from the address it sent to and needs no knowledge of the remap.
func imdsRemapSpecs() [][]string {
	if !imdsHostAddrsRemapped() {
		return nil
	}
	specs := make([][]string, 0, len(imdsCaptureAddrs))
	for i, guestAddr := range imdsCaptureAddrs {
		specs = append(specs, []string{
			"-i", IMDSEndpointPrefix + "+", "-d", guestAddr,
			"-m", "comment", "--comment", imdsRemapComment,
			"-j", "DNAT", "--to-destination", imdsHostAddrs[i],
		})
	}
	return specs
}

// EnsureIMDSRemapRules installs the DNATs when the endpoint addresses have been
// moved, and is a no-op otherwise — which is every bare-metal deployment.
//
// Inserted at the head and probed with -C first, for the reasons
// EnsureIMDSInputRule gives: a cloud image's nat table is not ours, and
// re-inserting per launch would blip IMDS for guests already running.
func EnsureIMDSRemapRules(ctx context.Context, r Runner) error {
	for _, spec := range imdsRemapSpecs() {
		if _, err := r.Run(ctx, "iptables", natRuleArgs("-C", "nat", "PREROUTING", spec)...); err == nil {
			continue
		}
		if out, err := r.Run(ctx, "iptables", natInsertArgs("nat", "PREROUTING", spec)...); err != nil {
			return fmt.Errorf("install IMDS remap rule: %s: %w", string(out), err)
		}
		slog.Info("host: installed IMDS remap rule", "guest_addr", spec[3], "host_addr", spec[len(spec)-1])
	}
	return nil
}

// RemoveIMDSRemapRules drops the DNATs; a missing rule is not an error.
func RemoveIMDSRemapRules(ctx context.Context, r Runner) {
	for _, spec := range imdsRemapSpecs() {
		if _, err := r.Run(ctx, "iptables", natRuleArgs("-D", "nat", "PREROUTING", spec)...); err != nil {
			slog.Debug("host: IMDS remap rule not present on delete", "err", err)
		}
	}
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
