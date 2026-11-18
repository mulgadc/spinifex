package host

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/netip"
	"slices"
	"strconv"
	"strings"
)

// EndpointProxyPort is where vpcd's endpoint proxy listens on every IMDS
// endpoint; the TPROXY rules deliver captured gateway-endpoint traffic to it.
const EndpointProxyPort = 8473

// The TPROXY mark and the policy-routing table and priority that deliver marked
// packets locally. The table sits below 256, clear of the hashed per-tap IMDS
// reply tables, and of the reserved 253-255.
const (
	EndpointCaptureMark     = "0x8473"
	EndpointCaptureTable    = "84"
	EndpointCapturePriority = "8473"
)

// Chains owned by the capture rules, each reached by one jump from its builtin
// chain, so reconciling is a diff of our own chain and removal is a flush.
const (
	endpointRawPreChain = "SPX-VPCE-PRE"
	endpointRawOutChain = "SPX-VPCE-OUT"
	endpointMangleChain = "SPX-VPCE-TPROXY"
	endpointProbeChain  = "SPX-VPCE-PROBE"
	endpointComment     = "spinifex-vpce"
)

// endpointChain is one owned chain: where it lives, how it is reached, and the
// rule it holds for each entry.
type endpointChain struct {
	table, builtin, chain string
	jump                  []string
	rule                  func(EndpointEntry) []string
	// keys name the address and port options the chain's rules match on.
	addrOpt, portOpt string
}

var endpointChains = []endpointChain{
	{
		table: "raw", builtin: "PREROUTING", chain: endpointRawPreChain,
		jump:    []string{"-i", IMDSEndpointPrefix + "+", "-m", "comment", "--comment", endpointComment, "-j", endpointRawPreChain},
		addrOpt: "-d", portOpt: "--dport",
		rule: func(e EndpointEntry) []string {
			return []string{"-i", IMDSEndpointPrefix + "+", "-p", "tcp", "-d", e.Addr.String() + "/32",
				"--dport", strconv.Itoa(int(e.Port)), "-j", "CT", "--notrack"}
		},
	},
	{
		table: "raw", builtin: "OUTPUT", chain: endpointRawOutChain,
		jump:    []string{"-o", IMDSEndpointPrefix + "+", "-m", "comment", "--comment", endpointComment, "-j", endpointRawOutChain},
		addrOpt: "-s", portOpt: "--sport",
		rule: func(e EndpointEntry) []string {
			return []string{"-o", IMDSEndpointPrefix + "+", "-p", "tcp", "-s", e.Addr.String() + "/32",
				"--sport", strconv.Itoa(int(e.Port)), "-j", "CT", "--notrack"}
		},
	},
	{
		table: "mangle", builtin: "PREROUTING", chain: endpointMangleChain,
		jump:    []string{"-i", IMDSEndpointPrefix + "+", "-m", "comment", "--comment", endpointComment, "-j", endpointMangleChain},
		addrOpt: "-d", portOpt: "--dport",
		rule: func(e EndpointEntry) []string {
			return append([]string{"-i", IMDSEndpointPrefix + "+", "-p", "tcp", "-d", e.Addr.String() + "/32",
				"--dport", strconv.Itoa(int(e.Port))}, tproxyTarget()...)
		},
	},
}

func tproxyTarget() []string {
	return []string{"-j", "TPROXY", "--on-ip", "0.0.0.0", "--on-port", strconv.Itoa(EndpointProxyPort),
		"--tproxy-mark", EndpointCaptureMark}
}

// EnsureEndpointCapture makes the host-wide capture rules match exactly entries:
// endpoint traffic arriving on an IMDS endpoint is left untracked and delivered
// to the proxy. An empty set removes the rules, the chains and the policy route.
func EnsureEndpointCapture(ctx context.Context, r Runner, entries []EndpointEntry) error {
	want, err := normaliseEntries(entries)
	if err != nil {
		return err
	}
	if len(want) == 0 {
		return removeEndpointCapture(ctx, r)
	}
	// Local delivery first, so a packet the TPROXY rule marks always has a route.
	if err := ensureEndpointPolicyRoute(ctx, r); err != nil {
		return err
	}
	for _, c := range endpointChains {
		if err := c.reconcile(ctx, r, want); err != nil {
			return err
		}
	}
	slog.Debug("host: endpoint capture reconciled", "entries", len(want))
	return nil
}

// reconcile diffs the chain against want, so an entry kept across passes is
// never removed and re-added, then makes sure the chain is reached.
func (c endpointChain) reconcile(ctx context.Context, r Runner, want []EndpointEntry) error {
	// -N fails when the chain exists, which is the usual case.
	_, _ = r.Run(ctx, "iptables", "-t", c.table, "-N", c.chain)
	out, err := r.Run(ctx, "iptables", "-t", c.table, "-S", c.chain)
	if err != nil {
		return fmt.Errorf("list %s %s: %w", c.table, c.chain, err)
	}
	var have []EndpointEntry
	for line := range strings.SplitSeq(string(out), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 || fields[0] != "-A" {
			continue
		}
		e, ok := c.parse(fields)
		if ok && slices.Contains(want, e) && !slices.Contains(have, e) {
			have = append(have, e)
			continue
		}
		if out, err := r.Run(ctx, "iptables", append([]string{"-t", c.table, "-D"}, fields[1:]...)...); err != nil {
			return fmt.Errorf("remove %s %s rule: %s: %w", c.table, c.chain, string(out), err)
		}
	}
	for _, e := range want {
		if slices.Contains(have, e) {
			continue
		}
		if out, err := r.Run(ctx, "iptables", natRuleArgs("-A", c.table, c.chain, c.rule(e))...); err != nil {
			return fmt.Errorf("add %s %s rule for %s: %s: %w", c.table, c.chain, e, string(out), err)
		}
	}
	if _, err := r.Run(ctx, "iptables", natRuleArgs("-C", c.table, c.builtin, c.jump)...); err == nil {
		return nil
	}
	if out, err := r.Run(ctx, "iptables", natInsertArgs(c.table, c.builtin, c.jump)...); err != nil {
		return fmt.Errorf("install %s %s jump to %s: %s: %w", c.table, c.builtin, c.chain, string(out), err)
	}
	return nil
}

// parse reads the entry an `iptables -S` rule of this chain matches. A rule
// whose target is not the one this chain installs parses as no entry.
func (c endpointChain) parse(fields []string) (EndpointEntry, bool) {
	var addr, port, onPort, mark string
	for i := 0; i+1 < len(fields); i++ {
		switch fields[i] {
		case c.addrOpt:
			addr = strings.TrimSuffix(fields[i+1], "/32")
		case c.portOpt:
			port = fields[i+1]
		case "--on-port":
			onPort = fields[i+1]
		case "--tproxy-mark":
			mark = strings.TrimSuffix(fields[i+1], "/0xffffffff")
		}
	}
	if c.chain == endpointMangleChain {
		if onPort != strconv.Itoa(EndpointProxyPort) || mark != EndpointCaptureMark {
			return EndpointEntry{}, false
		}
	} else if !strings.Contains(strings.Join(fields, " "), "-j CT --notrack") {
		return EndpointEntry{}, false
	}
	a, err := netip.ParseAddr(addr)
	if err != nil {
		return EndpointEntry{}, false
	}
	p, err := strconv.ParseUint(port, 10, 16)
	if err != nil {
		return EndpointEntry{}, false
	}
	return EndpointEntry{Addr: a, Port: uint16(p)}, true
}

var endpointRuleSelector = []string{"fwmark", EndpointCaptureMark, "lookup", EndpointCaptureTable}

// ensureEndpointPolicyRoute routes TPROXY-marked packets to local delivery.
// The rule is probed before adding, since `ip rule add` appends duplicates and
// a delete-then-add would briefly misroute captured packets.
func ensureEndpointPolicyRoute(ctx context.Context, r Runner) error {
	if out, err := r.Run(ctx, "ip", "route", "replace", "local", "0.0.0.0/0", "dev", "lo", "table", EndpointCaptureTable); err != nil {
		return fmt.Errorf("install endpoint capture route (table %s): %s: %w", EndpointCaptureTable, string(out), err)
	}
	out, err := r.Run(ctx, "ip", "rule", "show")
	if err != nil {
		return fmt.Errorf("list ip rules: %w", err)
	}
	for line := range strings.SplitSeq(string(out), "\n") {
		if strings.Contains(line, strings.Join(endpointRuleSelector, " ")) {
			return nil
		}
	}
	args := append(append([]string{"rule", "add"}, endpointRuleSelector...), "priority", EndpointCapturePriority)
	if out, err := r.Run(ctx, "ip", args...); err != nil {
		return fmt.Errorf("install endpoint capture rule: %s: %w", string(out), err)
	}
	return nil
}

// maxEndpointRuleDeletes bounds the duplicate-removal loops, so a delete that
// keeps succeeding cannot spin forever.
const maxEndpointRuleDeletes = 16

// removeEndpointCapture takes every capture rule, chain and the policy route
// away. Each step runs whatever the others did, and missing state is not an error.
func removeEndpointCapture(ctx context.Context, r Runner) error {
	var errs []error
	for _, c := range endpointChains {
		for range maxEndpointRuleDeletes {
			if _, err := r.Run(ctx, "iptables", natRuleArgs("-D", c.table, c.builtin, c.jump)...); err != nil {
				break
			}
		}
		if _, err := r.Run(ctx, "iptables", "-t", c.table, "-S", c.chain); err != nil {
			continue // already gone
		}
		if out, err := r.Run(ctx, "iptables", "-t", c.table, "-F", c.chain); err != nil {
			errs = append(errs, fmt.Errorf("flush %s %s: %s: %w", c.table, c.chain, string(out), err))
			continue
		}
		if out, err := r.Run(ctx, "iptables", "-t", c.table, "-X", c.chain); err != nil {
			errs = append(errs, fmt.Errorf("delete %s %s: %s: %w", c.table, c.chain, string(out), err))
		}
	}
	for range maxEndpointRuleDeletes {
		if _, err := r.Run(ctx, "ip", append([]string{"rule", "del"}, endpointRuleSelector...)...); err != nil {
			break
		}
	}
	if out, err := r.Run(ctx, "ip", "route", "flush", "table", EndpointCaptureTable); err != nil {
		errs = append(errs, fmt.Errorf("flush endpoint capture table %s: %s: %w", EndpointCaptureTable, string(out), err))
	}
	return errors.Join(errs...)
}

// ProbeEndpointCapture reports whether this kernel can install a TPROXY rule,
// by adding one that matches nothing to an unreferenced chain and removing it.
// It installs no capture rule and leaves nothing behind.
func ProbeEndpointCapture(ctx context.Context, r Runner) error {
	// A chain left by an interrupted probe is reused; the deferred flush empties it.
	_, _ = r.Run(ctx, "iptables", "-t", "mangle", "-N", endpointProbeChain)
	defer func() {
		_, _ = r.Run(ctx, "iptables", "-t", "mangle", "-F", endpointProbeChain)
		_, _ = r.Run(ctx, "iptables", "-t", "mangle", "-X", endpointProbeChain)
	}()
	spec := append([]string{"-p", "tcp", "-d", "0.0.0.0/32", "--dport", "1"}, tproxyTarget()...)
	if out, err := r.Run(ctx, "iptables", natRuleArgs("-A", "mangle", endpointProbeChain, spec)...); err != nil {
		return fmt.Errorf("TPROXY capture unavailable: %s: %w", strings.TrimSpace(string(out)), err)
	}
	return nil
}
