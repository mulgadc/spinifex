package host

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeNetfilter models the iptables chains and ip rules the capture code drives,
// so a reconcile is judged by the state it leaves and the changes it made.
type fakeNetfilter struct {
	chains map[string][]string // "table/chain" -> rule specs
	rules  []string            // ip rule selectors
	routes map[string]bool     // table -> local default present
	calls  []string
	failOn string // a call prefix that fails
}

func newFakeNetfilter() *fakeNetfilter {
	f := &fakeNetfilter{chains: map[string][]string{}, routes: map[string]bool{}}
	for _, c := range []string{"raw/PREROUTING", "raw/OUTPUT", "mangle/PREROUTING"} {
		f.chains[c] = nil
	}
	return f
}

var _ Runner = (*fakeNetfilter)(nil)

func (f *fakeNetfilter) Run(_ context.Context, name string, args ...string) ([]byte, error) {
	call := strings.Join(append([]string{name}, args...), " ")
	f.calls = append(f.calls, call)
	if f.failOn != "" && strings.HasPrefix(call, f.failOn) {
		return []byte("injected"), errors.New("injected failure")
	}
	switch name {
	case "iptables":
		return f.iptables(args)
	case "ip":
		return f.ip(args)
	}
	return nil, fmt.Errorf("unexpected command %q", call)
}

func (f *fakeNetfilter) iptables(args []string) ([]byte, error) {
	table, op, chain := args[1], args[2], args[3]
	key := table + "/" + chain
	rules, exists := f.chains[key]
	spec := ""
	switch op {
	case "-A", "-C", "-D":
		spec = strings.Join(args[4:], " ")
	case "-I":
		spec = strings.Join(args[5:], " ")
	}
	switch op {
	case "-N":
		if exists {
			return nil, errors.New("chain exists")
		}
		f.chains[key] = nil
	case "-X":
		if !exists || len(rules) > 0 {
			return nil, errors.New("cannot delete")
		}
		delete(f.chains, key)
	case "-F":
		if !exists {
			return nil, errors.New("no chain")
		}
		f.chains[key] = nil
	case "-S":
		if !exists {
			return nil, errors.New("no chain")
		}
		out := "-N " + chain + "\n"
		for _, r := range rules {
			out += "-A " + chain + " " + r + "\n"
		}
		return []byte(out), nil
	case "-A":
		if !exists {
			return nil, errors.New("no chain")
		}
		f.chains[key] = append(rules, spec)
	case "-I":
		f.chains[key] = append([]string{spec}, rules...)
	case "-C":
		if !slices.Contains(rules, spec) {
			return nil, errors.New("no rule")
		}
	case "-D":
		i := slices.Index(rules, spec)
		if i < 0 {
			return nil, errors.New("no rule")
		}
		f.chains[key] = slices.Delete(rules, i, i+1)
	default:
		return nil, fmt.Errorf("unexpected iptables op %s", op)
	}
	return nil, nil
}

func (f *fakeNetfilter) ip(args []string) ([]byte, error) {
	switch {
	case args[0] == "rule" && args[1] == "show":
		out := "0:\tfrom all lookup local\n"
		for _, r := range f.rules {
			out += "8473:\tfrom all " + r + "\n"
		}
		return []byte(out), nil
	case args[0] == "rule" && args[1] == "add":
		f.rules = append(f.rules, strings.Join(args[2:6], " "))
	case args[0] == "rule" && args[1] == "del":
		i := slices.Index(f.rules, strings.Join(args[2:], " "))
		if i < 0 {
			return nil, errors.New("no rule")
		}
		f.rules = slices.Delete(f.rules, i, i+1)
	case args[0] == "route" && args[1] == "replace":
		f.routes[args[len(args)-1]] = true
	case args[0] == "route" && args[1] == "flush":
		delete(f.routes, args[len(args)-1])
	default:
		return nil, fmt.Errorf("unexpected ip %v", args)
	}
	return nil, nil
}

func (f *fakeNetfilter) callsWith(prefix string) []string {
	var out []string
	for _, c := range f.calls {
		if strings.HasPrefix(c, prefix) {
			out = append(out, c)
		}
	}
	return out
}

func entry(s string) EndpointEntry {
	ap := netip.MustParseAddrPort(s)
	return EndpointEntry{Addr: ap.Addr(), Port: ap.Port()}
}

// The capture rules of a set of entries: one notrack each way and one TPROXY per
// entry, each in its own chain reached by exactly one jump, and local delivery
// for the mark.
func TestEnsureEndpointCapture_InstallsTheCaptureForEachEntry(t *testing.T) {
	f := newFakeNetfilter()
	require.NoError(t, EnsureEndpointCapture(context.Background(), f,
		[]EndpointEntry{entry("192.0.2.11:8443"), entry("192.0.2.10:8443"), entry("192.0.2.10:8443")}))

	assert.Equal(t, []string{
		"-i ime-+ -p tcp -d 192.0.2.10/32 --dport 8443 -j CT --notrack",
		"-i ime-+ -p tcp -d 192.0.2.11/32 --dport 8443 -j CT --notrack",
	}, f.chains["raw/SPX-VPCE-PRE"])
	assert.Equal(t, []string{
		"-o ime-+ -p tcp -s 192.0.2.10/32 --sport 8443 -j CT --notrack",
		"-o ime-+ -p tcp -s 192.0.2.11/32 --sport 8443 -j CT --notrack",
	}, f.chains["raw/SPX-VPCE-OUT"])
	assert.Equal(t, []string{
		"-i ime-+ -p tcp -d 192.0.2.10/32 --dport 8443 -j TPROXY --on-ip 0.0.0.0 --on-port 8473 --tproxy-mark 0x8473",
		"-i ime-+ -p tcp -d 192.0.2.11/32 --dport 8443 -j TPROXY --on-ip 0.0.0.0 --on-port 8473 --tproxy-mark 0x8473",
	}, f.chains["mangle/SPX-VPCE-TPROXY"])
	assert.Equal(t, []string{"-i ime-+ -m comment --comment spinifex-vpce -j SPX-VPCE-PRE"}, f.chains["raw/PREROUTING"])
	assert.Equal(t, []string{"-o ime-+ -m comment --comment spinifex-vpce -j SPX-VPCE-OUT"}, f.chains["raw/OUTPUT"])
	assert.Equal(t, []string{"-i ime-+ -m comment --comment spinifex-vpce -j SPX-VPCE-TPROXY"}, f.chains["mangle/PREROUTING"])
	assert.Equal(t, []string{"fwmark 0x8473 lookup 84"}, f.rules)
	assert.True(t, f.routes["84"])

	// The local route exists before any packet can be marked.
	route := slices.Index(f.calls, "ip route replace local 0.0.0.0/0 dev lo table 84")
	firstTproxy := slices.IndexFunc(f.calls, func(c string) bool { return strings.Contains(c, "-j TPROXY") })
	assert.Less(t, route, firstTproxy)
}

// A pass that changes the set touches only what changed, so an entry kept across
// passes never loses its rules, and a repeated pass changes nothing.
func TestEnsureEndpointCapture_ReconcilesByDiff(t *testing.T) {
	f := newFakeNetfilter()
	ctx := context.Background()
	require.NoError(t, EnsureEndpointCapture(ctx, f, []EndpointEntry{entry("192.0.2.10:8443"), entry("192.0.2.12:8443")}))

	f.calls = nil
	require.NoError(t, EnsureEndpointCapture(ctx, f, []EndpointEntry{entry("192.0.2.10:8443"), entry("192.0.2.11:8443")}))
	for _, c := range append(f.callsWith("iptables -t raw -D"), f.callsWith("iptables -t mangle -D")...) {
		assert.NotContains(t, c, "192.0.2.10", "a kept entry's rule was removed")
		assert.Contains(t, c, "192.0.2.12")
	}
	assert.Len(t, f.callsWith("iptables -t raw -D"), 2)
	assert.Len(t, f.callsWith("iptables -t mangle -D"), 1)
	assert.Equal(t, []string{
		"-i ime-+ -p tcp -d 192.0.2.10/32 --dport 8443 -j CT --notrack",
		"-i ime-+ -p tcp -d 192.0.2.11/32 --dport 8443 -j CT --notrack",
	}, f.chains["raw/SPX-VPCE-PRE"])

	f.calls = nil
	require.NoError(t, EnsureEndpointCapture(ctx, f, []EndpointEntry{entry("192.0.2.11:8443"), entry("192.0.2.10:8443")}))
	for _, op := range []string{"-A", "-D", "-I"} {
		assert.Empty(t, f.callsWith("iptables -t raw "+op), op)
		assert.Empty(t, f.callsWith("iptables -t mangle "+op), op)
	}
	assert.Empty(t, f.callsWith("ip rule add"))
	assert.Len(t, f.chains["mangle/PREROUTING"], 1)
	assert.Len(t, f.rules, 1)
}

// A rule in an owned chain that this build would not write, such as one aimed
// at another proxy port, is replaced rather than counted as the entry's rule.
func TestEnsureEndpointCapture_ReplacesARuleWithAnotherTarget(t *testing.T) {
	f := newFakeNetfilter()
	f.chains["mangle/SPX-VPCE-TPROXY"] = []string{
		"-i ime-+ -p tcp -d 192.0.2.10/32 --dport 8443 -j TPROXY --on-ip 0.0.0.0 --on-port 9999 --tproxy-mark 0x8473/0xffffffff",
	}
	require.NoError(t, EnsureEndpointCapture(context.Background(), f, []EndpointEntry{entry("192.0.2.10:8443")}))
	assert.Equal(t, []string{
		"-i ime-+ -p tcp -d 192.0.2.10/32 --dport 8443 -j TPROXY --on-ip 0.0.0.0 --on-port 8473 --tproxy-mark 0x8473",
	}, f.chains["mangle/SPX-VPCE-TPROXY"])
}

// An empty set leaves the host as if no endpoint had ever been captured.
func TestEnsureEndpointCapture_EmptySetRemovesEverything(t *testing.T) {
	f := newFakeNetfilter()
	ctx := context.Background()
	require.NoError(t, EnsureEndpointCapture(ctx, f, []EndpointEntry{entry("192.0.2.10:8443")}))
	require.NoError(t, EnsureEndpointCapture(ctx, f, nil))

	for _, c := range []string{"raw/SPX-VPCE-PRE", "raw/SPX-VPCE-OUT", "mangle/SPX-VPCE-TPROXY"} {
		_, exists := f.chains[c]
		assert.False(t, exists, c)
	}
	for _, c := range []string{"raw/PREROUTING", "raw/OUTPUT", "mangle/PREROUTING"} {
		assert.Empty(t, f.chains[c], c)
	}
	assert.Empty(t, f.rules)
	assert.False(t, f.routes["84"])

	// Removing again, with nothing installed, is not an error.
	require.NoError(t, EnsureEndpointCapture(ctx, f, nil))
}

func TestEnsureEndpointCapture_RefusesBadEntries(t *testing.T) {
	for name, e := range map[string]EndpointEntry{
		"IPv6":        entry("[2001:db8::1]:8443"),
		"no port":     {Addr: netip.MustParseAddr("192.0.2.10")},
		"unspecified": entry("0.0.0.0:8443"),
	} {
		t.Run(name, func(t *testing.T) {
			f := newFakeNetfilter()
			require.Error(t, EnsureEndpointCapture(context.Background(), f, []EndpointEntry{e}))
			assert.Empty(t, f.calls, "a refused set must change nothing")
		})
	}
}

// The probe proves the kernel takes a TPROXY rule, and leaves no chain behind
// whether or not it does.
func TestProbeEndpointCapture(t *testing.T) {
	f := newFakeNetfilter()
	require.NoError(t, ProbeEndpointCapture(context.Background(), f))
	assert.Len(t, f.callsWith("iptables -t mangle -A SPX-VPCE-PROBE -p tcp -d 0.0.0.0/32 --dport 1 -j TPROXY"), 1)
	_, exists := f.chains["mangle/SPX-VPCE-PROBE"]
	assert.False(t, exists)
	assert.Empty(t, f.chains["mangle/PREROUTING"], "the probe chain must never be reachable")

	f = newFakeNetfilter()
	f.failOn = "iptables -t mangle -A SPX-VPCE-PROBE"
	err := ProbeEndpointCapture(context.Background(), f)
	require.ErrorContains(t, err, "TPROXY capture unavailable")
	_, exists = f.chains["mangle/SPX-VPCE-PROBE"]
	assert.False(t, exists)
}
