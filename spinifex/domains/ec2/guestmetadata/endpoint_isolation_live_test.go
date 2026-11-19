package guestmetadata

import (
	"bufio"
	"context"
	"io"
	"net"
	"net/netip"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/mulgadc/predastore/endpointproxy"
	"github.com/mulgadc/spinifex/spinifex/domains/ec2/projection"
	"github.com/mulgadc/spinifex/spinifex/domains/network/host"
	"github.com/mulgadc/spinifex/spinifex/runtime/compute/vm"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"
)

// Both live guests sit in VPCs with the same CIDR and the same address, so
// their connections to an entry carry the same 4-tuple.
var (
	liveGuestAddr   = netip.MustParseAddr("10.77.0.5")
	liveGuestPrefix = "10.77.0.0/24"
	liveGateway     = "10.77.0.1"
	liveGatewayMAC  = "02:00:00:77:00:01"
	liveEntry       = netip.MustParseAddrPort("192.0.2.10:8443")
	// A port of its own, clear of the real gate listening on every address.
	liveLocalEntry = netip.MustParseAddrPort("192.0.2.20:9443")
)

// requireLiveEndpointHost skips unless an operator opted in on a root shell of
// an OVS host whose vpcd is stopped, since vpcd's pass rewrites the capture
// rules these tests install.
func requireLiveEndpointHost(t *testing.T) host.Runner {
	t.Helper()
	if os.Getenv("SPINIFEX_HOST_LIVE") != "1" {
		t.Skip("set SPINIFEX_HOST_LIVE=1 to run against this host's OVS and netfilter")
	}
	if os.Geteuid() != 0 {
		t.Skip("needs root to bind transparent sockets and enter network namespaces")
	}
	for _, tool := range []string{"ovs-vsctl", "ovs-ofctl", "iptables", "ip"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s not installed", tool)
		}
	}
	if exec.Command("systemctl", "is-active", "--quiet", "spinifex-vpcd").Run() == nil {
		t.Skip("stop spinifex-vpcd first: its endpoint pass rewrites the capture rules this test installs")
	}
	r := host.NewExecRunner()
	ctx := context.Background()
	if _, err := r.Run(ctx, "ovs-vsctl", "br-exists", host.IMDSBridge); err != nil {
		t.Skipf("no %s on this host", host.IMDSBridge)
	}
	if _, err := r.Run(ctx, "iptables", "-t", "mangle", "-S", "SPX-VPCE-TPROXY"); err == nil {
		t.Skip("this host already captures endpoint traffic; refusing to disturb it")
	}
	require.NoError(t, host.EnsureIMDSInputRule(ctx, r))
	return r
}

func liveRun(t *testing.T, r host.Runner, name string, args ...string) {
	t.Helper()
	out, err := r.Run(context.Background(), name, args...)
	require.NoError(t, err, "%s %s: %s", name, strings.Join(args, " "), out)
}

// liveGuest is a guest stand-in: its tap is an OVS internal port moved into a
// network namespace of its own, behind the tap's real IMDS datapath.
type liveGuest struct {
	facts    *eniFacts
	vpceID   string
	ns       string
	endpoint string
}

func newLiveGuest(t *testing.T, r host.Runner, tag string) *liveGuest {
	t.Helper()
	ctx := context.Background()
	eniID := "eni-0e2e0150" + tag + "0001"
	g := &liveGuest{
		facts: &eniFacts{
			eniID: eniID, accountID: "000000000077", vpcID: "vpc-0e2e0150" + tag, subnetID: "subnet-0e2e0150" + tag,
			privateIP: liveGuestAddr.String(), securityGroupIDs: []string{"sg-0e2e0150" + tag},
		},
		vpceID:   "vpce-0e2e0150" + tag,
		ns:       "spxiso-" + tag,
		endpoint: host.IMDSEndpointName(eniID),
	}
	tap, mac := vm.TapDeviceName(eniID), "02:00:00:77:0"+tag[:1]+":05"
	d := host.IMDSTapDatapath{
		Tap: tap, Endpoint: g.endpoint, EndpointMAC: host.IMDSEndpointMAC(eniID),
		GuestMAC: mac, GatewayMAC: liveGatewayMAC,
	}
	t.Cleanup(func() {
		_ = host.RemoveTapReplyRouting(ctx, r, d)
		_ = host.RemoveTapDatapath(ctx, r, d)
		_, _ = r.Run(ctx, "ovs-vsctl", "--if-exists", "del-port", host.IMDSBridge, tap)
		_, _ = r.Run(ctx, "ip", "netns", "del", g.ns)
	})
	liveRun(t, r, "ovs-vsctl", "--may-exist", "add-port", host.IMDSBridge, tap, "--", "set", "Interface", tap, "type=internal")
	liveRun(t, r, "ip", "netns", "add", g.ns)
	liveRun(t, r, "ip", "link", "set", tap, "netns", g.ns)
	liveRun(t, r, "ip", "-n", g.ns, "link", "set", tap, "address", mac)
	liveRun(t, r, "ip", "-n", g.ns, "link", "set", "lo", "up")
	liveRun(t, r, "ip", "-n", g.ns, "link", "set", tap, "up")
	liveRun(t, r, "ip", "-n", g.ns, "addr", "add", liveGuestAddr.String()+"/24", "dev", tap)
	liveRun(t, r, "ip", "-n", g.ns, "route", "add", "default", "via", liveGateway)
	liveRun(t, r, "ip", "-n", g.ns, "neigh", "replace", liveGateway, "lladdr", liveGatewayMAC, "dev", tap, "nud", "permanent")
	require.NoError(t, host.InstallTapDatapath(ctx, r, d))
	require.NoError(t, host.InstallTapReplyRouting(ctx, r, d))
	return g
}

// dial connects from inside the guest's namespace, then returns the thread to
// the host's; a thread it cannot return stays locked, so Go retires it.
func (g *liveGuest) dial(network string, local, remote netip.AddrPort, timeout time.Duration) (net.Conn, error) {
	var c net.Conn
	var err error
	done := make(chan struct{})
	go func() {
		defer close(done)
		runtime.LockOSThread()
		hostNS, herr := os.Open("/proc/thread-self/ns/net")
		if herr != nil {
			err = herr
			return
		}
		defer hostNS.Close()
		guestNS, gerr := os.Open("/run/netns/" + g.ns)
		if gerr != nil {
			err = gerr
			return
		}
		defer guestNS.Close()
		if err = unix.Setns(int(guestNS.Fd()), unix.CLONE_NEWNET); err != nil {
			return
		}
		defer func() {
			if unix.Setns(int(hostNS.Fd()), unix.CLONE_NEWNET) == nil {
				runtime.UnlockOSThread()
			}
		}()
		d := net.Dialer{Timeout: timeout}
		if local.IsValid() {
			if network == "udp" {
				d.LocalAddr = net.UDPAddrFromAddrPort(local)
			} else {
				d.LocalAddr = net.TCPAddrFromAddrPort(local)
			}
		}
		c, err = d.Dial(network, remote.String())
	}()
	<-done
	return c, err
}

// liveGate stands in for the gate's endpoint listener: it reads the header,
// answers with the endpoint ID it carried, then echoes.
func liveGate(t *testing.T) (EndpointDialFunc, <-chan endpointproxy.Header) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = ln.Close() })
	headers := make(chan endpointproxy.Header, 4)
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				h, err := endpointproxy.Read(c)
				if err != nil {
					return
				}
				headers <- h
				if _, err := io.WriteString(c, h.VpceID+"\n"); err != nil {
					return
				}
				_, _ = io.Copy(c, c)
			}()
		}
	}()
	dial := func(ctx context.Context, _ netip.Addr) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, "tcp", ln.Addr().String())
	}
	return dial, headers
}

// serveTap binds the proxy on the guest's endpoint as vpcd's responder does.
func serveTap(t *testing.T, p *EndpointProxy, g *liveGuest) {
	t.Helper()
	ln, err := p.listen(t.Context(), g.endpoint)
	require.NoError(t, err)
	served := make(chan struct{})
	go func() {
		defer close(served)
		_ = p.serve(t.Context(), newLimitListener(ln, endpointProxyConnsPerTap), g.facts)
	}()
	t.Cleanup(func() {
		_ = ln.Close()
		<-served
	})
}

// serveIMDS answers the guest's metadata and DNS addresses on its endpoint
// with the endpoint's name, standing in for the tap responder.
func serveIMDS(t *testing.T, g *liveGuest) {
	t.Helper()
	meta, dns := host.IMDSHostAddrs()
	lc := net.ListenConfig{Control: bindToDeviceControl(g.endpoint)}
	ln, err := lc.Listen(t.Context(), "tcp4", net.JoinHostPort(meta, "80"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			_, _ = io.WriteString(c, "imds "+g.endpoint+"\n")
			_ = c.Close()
		}
	}()
	pc, err := lc.ListenPacket(t.Context(), "udp4", net.JoinHostPort(dns, "53"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = pc.Close() })
	go func() {
		buf := make([]byte, 512)
		for {
			_, from, err := pc.ReadFrom(buf)
			if err != nil {
				return
			}
			_, _ = pc.WriteTo([]byte("dns "+g.endpoint), from)
		}
	}()
}

func readLine(t *testing.T, c net.Conn) string {
	t.Helper()
	require.NoError(t, c.SetReadDeadline(time.Now().Add(5*time.Second)))
	line, err := bufio.NewReader(c).ReadString('\n')
	require.NoError(t, err)
	return strings.TrimSuffix(line, "\n")
}

// conntrackTo returns the host's conntrack entries for connections to dst.
func conntrackTo(t *testing.T, dst netip.AddrPort) []string {
	t.Helper()
	b, err := os.ReadFile("/proc/net/nf_conntrack")
	if os.IsNotExist(err) {
		t.Skip("conntrack is not loaded on this host")
	}
	require.NoError(t, err)
	var hits []string
	for l := range strings.SplitSeq(string(b), "\n") {
		if strings.Contains(l, "dst="+dst.Addr().String()+" ") && strings.Contains(l, "dport="+strconv.Itoa(int(dst.Port()))+" ") {
			hits = append(hits, l)
		}
	}
	return hits
}

// Two guests in overlapping VPCs on one host dial the same entry from the same
// address and port at once. Each reply reaches its own guest, each proxied
// connection carries its own endpoint, and neither is tracked.
func TestLive_EndpointProxyIsolatesOverlappingVPCs(t *testing.T) {
	r := requireLiveEndpointHost(t)
	ctx := context.Background()

	dial, headers := liveGate(t)
	p, err := NewEndpointProxy(EndpointProxyConfig{
		Port:   host.EndpointProxyPort,
		Egress: func(context.Context, string, []string, netip.AddrPort) (bool, error) { return true, nil },
		Dial:   dial,
		MaxAge: time.Minute,
	})
	require.NoError(t, err)
	p.refresh = func(_ context.Context, pinned *eniFacts) (*eniFacts, error) { return pinned, nil }
	t.Cleanup(p.shutdown)

	guests := []*liveGuest{newLiveGuest(t, r, "a"), newLiveGuest(t, r, "b")}
	view := EndpointView{Entries: []netip.AddrPort{liveEntry}, Endpoints: map[string]projection.GatewayEndpoint{}}
	for _, g := range guests {
		view.Endpoints[g.facts.subnetID] = projection.GatewayEndpoint{ID: g.vpceID, AccountID: g.facts.accountID, VpcID: g.facts.vpcID}
	}
	p.Publish(view)

	t.Cleanup(func() { _ = host.EnsureEndpointCapture(ctx, r, nil) })
	require.NoError(t, host.EnsureEndpointCapture(ctx, r, []host.EndpointEntry{{Addr: liveEntry.Addr(), Port: liveEntry.Port()}}))
	for _, g := range guests {
		serveTap(t, p, g)
		serveIMDS(t, g)
		require.NoError(t, host.EnsureEndpointDemux(ctx, r, g.facts.eniID, []host.EndpointEntry{{Addr: liveEntry.Addr(), Port: liveEntry.Port()}}))
	}

	src := netip.AddrPortFrom(liveGuestAddr, 40000)
	conns := make([]net.Conn, len(guests))
	for i, g := range guests {
		c, err := g.dial("tcp", src, liveEntry, 5*time.Second)
		require.NoError(t, err, "guest %s could not reach the entry", g.ns)
		t.Cleanup(func() { _ = c.Close() })
		conns[i] = c
	}
	// Both connections are open before either reads, so the two identical
	// tuples coexist on the host.
	for i, g := range guests {
		assert.Equal(t, g.vpceID, readLine(t, conns[i]), "guest %s got another guest's reply", g.ns)
		_, err := io.WriteString(conns[i], "ping "+g.ns+"\n")
		require.NoError(t, err)
		assert.Equal(t, "ping "+g.ns, readLine(t, conns[i]))
	}

	got := map[string]endpointproxy.Header{}
	for range guests {
		select {
		case h := <-headers:
			got[h.VpceID] = h
		case <-time.After(5 * time.Second):
			t.Fatal("the gate saw fewer connections than the guests opened")
		}
	}
	for _, g := range guests {
		h, ok := got[g.vpceID]
		require.True(t, ok, "no connection carried %s", g.vpceID)
		assert.Equal(t, src, h.Source)
		assert.Equal(t, liveEntry, h.Destination)
		assert.Equal(t, g.facts.vpcID, h.VpcID)
		t.Logf("guest %s: source %s, destination %s, header endpoint %s, vpc %s", g.ns, h.Source, h.Destination, h.VpceID, h.VpcID)
	}
	assert.Empty(t, conntrackTo(t, liveEntry), "an endpoint connection was tracked")

	// With an endpoint on the tap, metadata and DNS still answer each guest
	// from its own endpoint.
	for _, g := range guests {
		c, err := g.dial("tcp", netip.AddrPort{}, netip.MustParseAddrPort("169.254.169.254:80"), 5*time.Second)
		require.NoError(t, err, "guest %s lost IMDS", g.ns)
		assert.Equal(t, "imds "+g.endpoint, readLine(t, c))
		_ = c.Close()
		// IMDS is tracked as ever, so an empty read above means untracked.
		assert.NotEmpty(t, conntrackTo(t, netip.MustParseAddrPort("169.254.169.254:80")), "conntrack recorded no IMDS connection")

		u, err := g.dial("udp", netip.AddrPort{}, netip.MustParseAddrPort("169.254.169.253:53"), 5*time.Second)
		require.NoError(t, err)
		_, err = u.Write([]byte("q"))
		require.NoError(t, err)
		require.NoError(t, u.SetReadDeadline(time.Now().Add(5*time.Second)))
		buf := make([]byte, 64)
		n, err := u.Read(buf)
		require.NoError(t, err, "guest %s lost DNS", g.ns)
		assert.Equal(t, "dns "+g.endpoint, string(buf[:n]))
		_ = u.Close()
	}
}

// A tap with the demux but no capture rule hands the packet to the host stack,
// which owns the entry's address here as on a gate node; the connection must
// time out without the gate ever accepting it.
func TestLive_EndpointDemuxWithoutCaptureNeverConnects(t *testing.T) {
	r := requireLiveEndpointHost(t)
	ctx := context.Background()
	const gateDev = "spxisogate0"

	t.Cleanup(func() {
		_, _ = r.Run(ctx, "ip", "link", "del", gateDev)
		_, _ = r.Run(ctx, "ip", "route", "del", "blackhole", liveGuestPrefix)
	})
	liveRun(t, r, "ip", "link", "add", gateDev, "type", "dummy")
	liveRun(t, r, "ip", "addr", "add", liveLocalEntry.Addr().String()+"/32", "dev", gateDev)
	liveRun(t, r, "ip", "link", "set", gateDev, "up")
	// The gate's answer would otherwise leave by the default route.
	liveRun(t, r, "ip", "route", "add", "blackhole", liveGuestPrefix)

	ln, err := net.Listen("tcp", liveLocalEntry.String())
	require.NoError(t, err)
	t.Cleanup(func() { _ = ln.Close() })
	accepted := make(chan struct{}, 1)
	go func() {
		if c, err := ln.Accept(); err == nil {
			accepted <- struct{}{}
			_ = c.Close()
		}
	}()

	g := newLiveGuest(t, r, "c")
	require.NoError(t, host.EnsureEndpointCapture(ctx, r, nil))
	require.NoError(t, host.EnsureEndpointDemux(ctx, r, g.facts.eniID, []host.EndpointEntry{{Addr: liveLocalEntry.Addr(), Port: liveLocalEntry.Port()}}))

	c, err := g.dial("tcp", netip.AddrPortFrom(liveGuestAddr, 40000), liveLocalEntry, 3*time.Second)
	if c != nil {
		_ = c.Close()
	}
	require.Error(t, err, "a demuxed connection with no capture rule connected")
	var ne net.Error
	require.ErrorAs(t, err, &ne)
	assert.True(t, ne.Timeout(), "the connection must time out, not be refused: %v", err)
	select {
	case <-accepted:
		t.Fatal("the gate accepted a connection that bypassed the proxy")
	default:
	}
	t.Logf("guest %s: dial to %s without capture: %v", g.ns, liveLocalEntry, err)
}
