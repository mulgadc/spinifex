package host

import (
	"context"
	"fmt"
	"log/slog"
)

const natEgressComment = "spinifex-nat-egress"

// natEgressRules are the kernel rules giving routed-NAT VMs outbound WAN:
// masquerade the transit /24 out any uplink, and accept forwarded transit
// traffic even when the FORWARD policy is DROP (Docker/firewalld hosts).
var natEgressRules = []struct {
	table string
	chain string
	spec  []string
}{
	{"nat", "POSTROUTING", []string{
		"-s", NATTransitCIDR, "!", "-d", NATTransitCIDR,
		"-m", "comment", "--comment", natEgressComment, "-j", "MASQUERADE"}},
	{"filter", "FORWARD", []string{
		"-i", NATTransitHostEnd, "-s", NATTransitCIDR,
		"-m", "comment", "--comment", natEgressComment, "-j", "ACCEPT"}},
	{"filter", "FORWARD", []string{
		"-o", NATTransitHostEnd, "-m", "conntrack", "--ctstate", "RELATED,ESTABLISHED",
		"-m", "comment", "--comment", natEgressComment, "-j", "ACCEPT"}},
}

func natRuleArgs(op, table, chain string, spec []string) []string {
	args := []string{"-t", table, op, chain}
	return append(args, spec...)
}

// EnsureNATEgressRules idempotently installs the routed-NAT egress rules:
// probe with -C, append with -A only when missing. vpcd calls this on every
// start, so rules survive reboots without iptables-persistent.
func EnsureNATEgressRules(ctx context.Context, r Runner) error {
	for _, rule := range natEgressRules {
		if _, err := r.Run(ctx, "iptables", natRuleArgs("-C", rule.table, rule.chain, rule.spec)...); err == nil {
			continue
		}
		if out, err := r.Run(ctx, "iptables", natRuleArgs("-A", rule.table, rule.chain, rule.spec)...); err != nil {
			return fmt.Errorf("install NAT egress rule (%s %s): %s: %w", rule.table, rule.chain, string(out), err)
		}
		slog.Info("host: installed NAT egress rule", "table", rule.table, "chain", rule.chain)
	}
	return nil
}

// RemoveNATEgressRules deletes the routed-NAT egress rules; missing rules are
// not an error (teardown is idempotent).
func RemoveNATEgressRules(ctx context.Context, r Runner) {
	for _, rule := range natEgressRules {
		if _, err := r.Run(ctx, "iptables", natRuleArgs("-D", rule.table, rule.chain, rule.spec)...); err != nil {
			slog.Debug("host: NAT egress rule not present on delete", "table", rule.table, "chain", rule.chain, "err", err)
		}
	}
}
