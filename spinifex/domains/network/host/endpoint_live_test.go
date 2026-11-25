package host

import (
	"context"
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/mulgadc/spinifex/spinifex/runtime/compute/vm"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"
)

// requireLiveHost skips unless the operator opted in: these tests change the
// host's netfilter rules and br-imds flows, so they never run by default.
func requireLiveHost(t *testing.T) Runner {
	t.Helper()
	if os.Getenv("SPINIFEX_HOST_LIVE") != "1" {
		t.Skip("set SPINIFEX_HOST_LIVE=1 to run against this host's OVS and netfilter")
	}
	for _, tool := range []string{"ovs-vsctl", "ovs-ofctl", "iptables", "ip"} {
		if !toolInstalled(tool) {
			t.Skipf("%s not installed", tool)
		}
	}
	lockLiveHost(t)
	return NewExecRunner()
}

// liveHostLock is shared with the guestmetadata live tests, so packages run
// together never interleave changes to the host-wide capture rules.
const liveHostLock = "/run/lock/spinifex-endpoint-live.lock"

func lockLiveHost(t *testing.T) {
	t.Helper()
	f, err := os.OpenFile(liveHostLock, os.O_CREATE|os.O_RDWR, 0o600)
	require.NoError(t, err)
	require.NoError(t, unix.Flock(int(f.Fd()), unix.LOCK_EX))
	t.Cleanup(func() { _ = f.Close() })
}

// toolInstalled also looks in the sbin directories, which sudo searches but an
// unprivileged PATH often omits.
func toolInstalled(tool string) bool {
	if _, err := exec.LookPath(tool); err == nil {
		return true
	}
	for _, dir := range []string{"/usr/sbin", "/sbin"} {
		if _, err := os.Stat(dir + "/" + tool); err == nil {
			return true
		}
	}
	return false
}

func liveLines(t *testing.T, r Runner, name string, args ...string) []string {
	t.Helper()
	out, err := r.Run(context.Background(), name, args...)
	require.NoError(t, err)
	var lines []string
	for l := range strings.SplitSeq(strings.TrimSpace(string(out)), "\n") {
		if l != "" {
			lines = append(lines, l)
		}
	}
	return lines
}

// passthroughRecorder passes commands through and keeps their command lines.
type passthroughRecorder struct {
	Runner

	calls []string
}

func (r *passthroughRecorder) Run(ctx context.Context, name string, args ...string) ([]byte, error) {
	r.calls = append(r.calls, strings.Join(append([]string{name}, args...), " "))
	return r.Runner.Run(ctx, name, args...)
}

func chainRules(t *testing.T, r Runner, table, chain string) []string {
	var rules []string
	for _, l := range liveLines(t, r, "iptables", "-t", table, "-S", chain) {
		if strings.HasPrefix(l, "-A ") {
			rules = append(rules, l)
		}
	}
	return rules
}

func chainExists(r Runner, table, chain string) bool {
	_, err := r.Run(context.Background(), "iptables", "-t", table, "-S", chain)
	return err == nil
}

func TestLive_EndpointCapture(t *testing.T) {
	r := requireLiveHost(t)
	ctx := context.Background()
	if chainExists(r, "mangle", endpointMangleChain) {
		t.Skip("this host already captures endpoint traffic; refusing to disturb it")
	}
	t.Cleanup(func() { _ = EnsureEndpointCapture(ctx, r, nil) })

	both := []EndpointEntry{entry("192.0.2.10:8443"), entry("192.0.2.11:8443")}
	require.NoError(t, EnsureEndpointCapture(ctx, r, both))
	// Parsing the kernel's own listing must recognise every rule, or each pass
	// would churn them and cut established endpoint connections.
	rec := &passthroughRecorder{Runner: r}
	require.NoError(t, EnsureEndpointCapture(ctx, rec, both))
	for _, c := range rec.calls {
		for _, op := range []string{" -A ", " -D ", " -I ", "rule add"} {
			assert.NotContains(t, c, op, "a repeated pass changed state")
		}
	}
	assert.Len(t, chainRules(t, r, "raw", endpointRawPreChain), 2)
	assert.Len(t, chainRules(t, r, "raw", endpointRawOutChain), 2)
	assert.Len(t, chainRules(t, r, "mangle", endpointMangleChain), 2)
	for _, c := range []struct{ table, builtin, chain string }{
		{"raw", "PREROUTING", endpointRawPreChain},
		{"raw", "OUTPUT", endpointRawOutChain},
		{"mangle", "PREROUTING", endpointMangleChain},
	} {
		jumps := 0
		for _, l := range chainRules(t, r, c.table, c.builtin) {
			if strings.HasSuffix(l, "-j "+c.chain) {
				jumps++
			}
		}
		assert.Equal(t, 1, jumps, "%s %s jumps to %s", c.table, c.builtin, c.chain)
	}
	rules := strings.Join(liveLines(t, r, "ip", "rule", "show"), "\n")
	assert.Equal(t, 1, strings.Count(rules, "fwmark 0x8473 lookup 84"))
	assert.Contains(t, strings.Join(liveLines(t, r, "ip", "route", "show", "table", "84"), "\n"), "local default dev lo")

	require.NoError(t, EnsureEndpointCapture(ctx, r, []EndpointEntry{entry("192.0.2.11:8443")}))
	pre := chainRules(t, r, "raw", endpointRawPreChain)
	require.Len(t, pre, 1)
	assert.Contains(t, pre[0], "192.0.2.11/32")
	assert.Len(t, chainRules(t, r, "mangle", endpointMangleChain), 1)

	require.NoError(t, EnsureEndpointCapture(ctx, r, nil))
	for _, c := range [][2]string{{"raw", endpointRawPreChain}, {"raw", endpointRawOutChain}, {"mangle", endpointMangleChain}} {
		assert.False(t, chainExists(r, c[0], c[1]), "%s %s left behind", c[0], c[1])
	}
	assert.NotContains(t, strings.Join(liveLines(t, r, "ip", "rule", "show"), "\n"), "fwmark 0x8473")
	assert.Empty(t, liveLines(t, r, "ip", "route", "show", "table", "84"))
}

func TestLive_ProbeEndpointCapture(t *testing.T) {
	r := requireLiveHost(t)
	require.NoError(t, ProbeEndpointCapture(context.Background(), r))
	assert.False(t, chainExists(r, "mangle", endpointProbeChain), "the probe left its chain behind")
}

// The demux follows the entry set under its own cookie on a real br-imds, and
// neither its changes nor its removal touch the tap's IMDS flows.
func TestLive_EndpointDemux(t *testing.T) {
	r := requireLiveHost(t)
	ctx := context.Background()
	const eni = "eni-0e2e0live0test"
	tap, endpoint := vm.TapDeviceName(eni), IMDSEndpointName(eni)
	for _, port := range []string{tap, endpoint} {
		_, err := r.Run(ctx, "ovs-vsctl", "--may-exist", "add-port", IMDSBridge, port, "--", "set", "Interface", port, "type=internal")
		require.NoError(t, err)
	}
	imdsCookie, epCookie := imdsFlowCookie(endpoint), endpointFlowCookie(endpoint)
	t.Cleanup(func() {
		_ = clearIMDSFlowsByCookie(ctx, r, imdsCookie)
		_ = clearIMDSFlowsByCookie(ctx, r, epCookie)
		for _, port := range []string{tap, endpoint} {
			_, _ = r.Run(ctx, "ovs-vsctl", "--if-exists", "del-port", IMDSBridge, port)
		}
	})
	require.NoError(t, installIMDSFlow(ctx, r, imdsCookie,
		"table=0,priority=200,in_port="+tap+",ip,nw_dst=169.254.169.254,actions=output:"+endpoint))

	flows := func(cookie string) []EndpointEntry {
		out, err := r.Run(ctx, "ovs-ofctl", "--no-stats", "dump-flows", IMDSBridge, "cookie="+cookie+"/-1")
		require.NoError(t, err)
		if cookie == imdsCookie {
			return make([]EndpointEntry, strings.Count(string(out), "cookie="))
		}
		got, err := parseEndpointDemuxFlows(out)
		require.NoError(t, err)
		return got
	}

	require.NoError(t, EnsureEndpointDemux(ctx, r, eni, []EndpointEntry{entry("192.0.2.10:8443"), entry("192.0.2.11:8443")}))
	assert.ElementsMatch(t, []EndpointEntry{entry("192.0.2.10:8443"), entry("192.0.2.11:8443")}, flows(epCookie))

	rec := &passthroughRecorder{Runner: r}
	require.NoError(t, EnsureEndpointDemux(ctx, rec, eni, []EndpointEntry{entry("192.0.2.10:8443"), entry("192.0.2.11:8443")}))
	for _, c := range rec.calls {
		assert.NotContains(t, c, "del-flows", "a repeated pass removed a wanted flow")
	}

	require.NoError(t, EnsureEndpointDemux(ctx, r, eni, []EndpointEntry{entry("192.0.2.11:8443"), entry("192.0.2.12:8443")}))
	assert.ElementsMatch(t, []EndpointEntry{entry("192.0.2.11:8443"), entry("192.0.2.12:8443")}, flows(epCookie))
	assert.Len(t, flows(imdsCookie), 1, "IMDS flows changed by a demux pass")

	require.NoError(t, RemoveEndpointDemux(ctx, r, eni))
	assert.Empty(t, flows(epCookie))
	assert.Len(t, flows(imdsCookie), 1, "IMDS flows removed with the demux")

	require.NoError(t, EnsureEndpointDemux(ctx, r, eni, []EndpointEntry{entry("192.0.2.10:8443")}))
	require.NoError(t, RemoveTapDatapath(ctx, r, IMDSTapDatapath{Endpoint: endpoint}))
	assert.Empty(t, flows(epCookie), "tap teardown left endpoint flows behind")
}
