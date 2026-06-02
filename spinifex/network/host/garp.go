package host

import (
	"context"
	"fmt"
	"strings"
)

// InjectGARP triggers ovn-controller on the local chassis to emit a
// gratuitous ARP for every IP associated with logicalPort. Wraps
// `ovn-appctl -t ovn-controller inject-garp <port>` (OVN 22.09+).
//
// Required after AddEIP when the external IP is being recycled: OVN emits
// its automatic GARP only on LSP binding-chassis migration, so a fresh
// external_ip-to-LRP rebind on the same chassis leaves upstream ARP caches
// pointing at the prior chassis-redirect MAC until the kernel ARP timeout
// (60-300s) expires.
//
// Caller chooses the correct port:
//   - distributed dnat_and_snat (logical_port + external_mac set): the LSP
//     name (e.g. "port-eni-abc"). GARP fires from the LSP-binding chassis
//     with external_mac.
//   - centralized dnat_and_snat (no logical_port): the chassisredirect
//     port for the VPC's gateway LRP ("cr-gw-<vpcID>"). GARP fires from
//     the gw chassis with the LRP MAC.
//
// L0 method (ADR-0006 S2) — only network/host/ may shell out to host tools.
// Best-effort: callers must treat errors as warnings, not failures, because
// inject-garp may fail when ovn-controller is restarting or the port binding
// has not yet propagated to SBDB.
func InjectGARP(ctx context.Context, runner Runner, logicalPort string) error {
	if logicalPort == "" {
		return fmt.Errorf("host.InjectGARP: logicalPort required")
	}
	if runner == nil {
		runner = NewExecRunner()
	}
	out, err := runner.Run(ctx, "ovn-appctl", "-t", "ovn-controller", "inject-garp", logicalPort)
	if err != nil {
		return fmt.Errorf("ovn-appctl inject-garp %s: %s: %w", logicalPort, strings.TrimSpace(string(out)), err)
	}
	return nil
}
