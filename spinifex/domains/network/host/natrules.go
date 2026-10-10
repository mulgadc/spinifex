package host

import (
	"context"
	"fmt"
	"log/slog"
)

const natEgressComment = "spinifex-nat-egress"

// maxDuplicateRules caps the drain loop. iptables has no delete-all, and a
// chain holding more copies than this has a problem no reconcile should paper
// over.
const maxDuplicateRules = 8

// natEgressRules are the kernel rules giving routed-NAT VMs outbound WAN:
// masquerade the transit /24 out any uplink, and accept forwarded transit
// traffic even when FORWARD would otherwise refuse it.
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

// natInsertArgs builds an insert at the head of the chain.
func natInsertArgs(table, chain string, spec []string) []string {
	args := []string{"-t", table, "-I", chain, "1"}
	return append(args, spec...)
}

// EnsureNATEgressRules installs the routed-NAT egress rules at the head of
// their chains, removing any existing copies first. vpcd calls this on every
// start, so rules survive reboots without iptables-persistent.
//
// Appending is not enough and probing with -C is worse than useless: Ubuntu
// ships a catch-all REJECT in FORWARD, so an appended ACCEPT never matches,
// and a rule an earlier build appended still satisfies -C — which would leave
// a broken host broken across an upgrade. Every spec carries our comment, so
// the drain only ever deletes our own rules.
func EnsureNATEgressRules(ctx context.Context, r Runner) error {
	for _, rule := range natEgressRules {
		for range maxDuplicateRules {
			if _, err := r.Run(ctx, "iptables", natRuleArgs("-D", rule.table, rule.chain, rule.spec)...); err != nil {
				break
			}
		}
		if out, err := r.Run(ctx, "iptables", natInsertArgs(rule.table, rule.chain, rule.spec)...); err != nil {
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
