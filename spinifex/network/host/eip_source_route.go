package host

import (
	"context"
	"fmt"
	"net/netip"
	"strings"
)

// eipSourceRule is the policy-routing rule an EIP needs when the host already
// source-routes the uplink address that shares the EIP's subnet.
type eipSourceRule struct {
	Table    string
	Priority string
}

// uplinkSourceRuleFor finds the policy-routing rule governing replies that
// leave with eip as their source, by looking for a `from <addr> lookup <table>`
// rule whose address sits in the same subnet as the EIP.
//
// A cloud VNIC accepts only the addresses registered on it, so a reply sourced
// from an EIP has to leave through the same interface as the uplink address it
// shares a subnet with. Where the host enforces that with a source rule, every
// EIP on that interface needs the same rule; where no such rule exists there is
// nothing to mirror and nothing to do.
//
// The rule is the selector, not the interface list, because on OCI both VNICs
// sit in one subnet and "the interface owning the EIP's subnet" names two of
// them. The rule names exactly the one whose replies the operator has already
// pinned.
func uplinkSourceRuleFor(ctx context.Context, r Runner, eip string) (eipSourceRule, bool, error) {
	addr, err := netip.ParseAddr(eip)
	if err != nil {
		return eipSourceRule{}, false, fmt.Errorf("parse EIP %q: %w", eip, err)
	}

	prefixes, err := hostPrefixes(ctx, r)
	if err != nil {
		return eipSourceRule{}, false, err
	}

	out, err := r.Run(ctx, "ip", "rule", "show")
	if err != nil {
		return eipSourceRule{}, false, fmt.Errorf("list ip rules: %s: %w", string(out), err)
	}

	var found []eipSourceRule
	var owners []string
	for line := range strings.SplitSeq(string(out), "\n") {
		prio, from, table, ok := parseIPRule(line)
		if !ok {
			continue
		}
		prefix, held := prefixes[from]
		if !held || from == eip || !prefix.Contains(addr) {
			continue
		}
		found = append(found, eipSourceRule{Table: table, Priority: prio})
		owners = append(owners, from+" -> table "+table)
	}

	switch len(found) {
	case 0:
		return eipSourceRule{}, false, nil
	case 1:
		return found[0], true, nil
	default:
		// Picking one would send this EIP's replies out an interface that does
		// not hold its address, which the provider drops without a word.
		return eipSourceRule{}, false, fmt.Errorf(
			"source routing for %s is ambiguous: %s", eip, strings.Join(owners, ", "))
	}
}

// hostPrefixes maps each global IPv4 the host holds to the prefix it was
// configured with, so a rule's source address can be tested for the EIP's
// subnet without assuming a mask.
func hostPrefixes(ctx context.Context, r Runner) (map[string]netip.Prefix, error) {
	out, err := r.Run(ctx, "ip", "-4", "-o", "addr", "show", "scope", "global")
	if err != nil {
		return nil, fmt.Errorf("list host addresses: %s: %w", string(out), err)
	}
	prefixes := map[string]netip.Prefix{}
	for line := range strings.SplitSeq(string(out), "\n") {
		fields := strings.Fields(line)
		for i := 0; i < len(fields)-1; i++ {
			if fields[i] != "inet" {
				continue
			}
			prefix, perr := netip.ParsePrefix(fields[i+1])
			if perr != nil {
				continue
			}
			prefixes[prefix.Addr().String()] = prefix
		}
	}
	return prefixes, nil
}

// parseIPRule pulls the priority, source address and table out of one line of
// `ip rule show`. Source-address rules only; anything else reports !ok.
func parseIPRule(line string) (priority, from, table string, ok bool) {
	fields := strings.Fields(line)
	if len(fields) < 4 || !strings.HasSuffix(fields[0], ":") {
		return "", "", "", false
	}
	priority = strings.TrimSuffix(fields[0], ":")
	var oif bool
	for i := 1; i < len(fields)-1; i++ {
		switch fields[i] {
		case "from":
			// The kernel prints a host address bare and a network with its
			// prefix; both mean the same thing for a /32.
			from = strings.TrimSuffix(fields[i+1], "/32")
		case "oif", "iif":
			oif = true
		case "lookup":
			table = fields[i+1]
		}
	}
	if oif || from == "" || from == "all" || table == "" {
		return "", "", "", false
	}
	return priority, from, table, true
}

// EnsureEIPSourceRoute mirrors the uplink's source-routing rule onto eip, so a
// reply carrying the EIP leaves through the interface that holds the address.
// No-op on a host that does not source-route its uplink. Idempotent: the rule
// is deleted before it is added, because `ip rule add` appends duplicates.
func EnsureEIPSourceRoute(ctx context.Context, r Runner, eip string) error {
	rule, found, err := uplinkSourceRuleFor(ctx, r, eip)
	if err != nil || !found {
		return err
	}
	_, _ = r.Run(ctx, "ip", "rule", "del", "from", eip+"/32", "lookup", rule.Table)
	args := []string{"rule", "add", "from", eip + "/32", "lookup", rule.Table}
	if rule.Priority != "" {
		args = append(args, "priority", rule.Priority)
	}
	if out, err := r.Run(ctx, "ip", args...); err != nil {
		return fmt.Errorf("install source route for %s via table %s: %s: %w", eip, rule.Table, string(out), err)
	}
	return nil
}

// RemoveEIPSourceRoute drops the rule EnsureEIPSourceRoute installed. Absence
// is not an error: the address may never have needed one.
func RemoveEIPSourceRoute(ctx context.Context, r Runner, eip string) error {
	rule, found, err := uplinkSourceRuleFor(ctx, r, eip)
	if err != nil || !found {
		return err
	}
	_, _ = r.Run(ctx, "ip", "rule", "del", "from", eip+"/32", "lookup", rule.Table)
	return nil
}
