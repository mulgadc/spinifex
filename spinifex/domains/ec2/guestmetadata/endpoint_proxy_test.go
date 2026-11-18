package guestmetadata

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"io"
	"math/big"
	"net"
	"net/netip"
	"os"
	"strconv"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/mulgadc/predastore/endpointproxy"
	"github.com/mulgadc/spinifex/spinifex/domains/ec2/projection"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var (
	epTestGuest   = netip.MustParseAddr("10.0.1.5")
	epTestEntry   = netip.MustParseAddrPort("192.0.2.10:8443")
	epTestOther   = netip.MustParseAddrPort("192.0.2.99:8443")
	epTestSubnet  = "subnet-0a1b2c3d"
	epTestVPC     = "vpc-0a1b2c3d4e"
	epTestAccount = "123456789012"
	epTestVpce    = "vpce-0123456789abcdef0"
)

func testEndpoint() projection.GatewayEndpoint {
	return projection.GatewayEndpoint{ID: epTestVpce, AccountID: epTestAccount, VpcID: epTestVPC, ServiceName: "com.amazonaws.ap-southeast-2.s3"}
}

func testView() EndpointView {
	return EndpointView{
		Entries:   []netip.AddrPort{epTestEntry},
		Endpoints: map[string]projection.GatewayEndpoint{epTestSubnet: testEndpoint()},
	}
}

// upstream is a fake gate endpoint listener: TLS, reads one PROXY header, then
// echoes. Headers it read arrive on headers.
type upstream struct {
	ln      net.Listener
	pool    *x509.CertPool
	headers chan endpointproxy.Header
}

func startUpstream(t *testing.T) *upstream {
	t.Helper()
	cert, pool := selfSigned(t)
	ln, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS13})
	require.NoError(t, err)
	u := &upstream{ln: ln, pool: pool, headers: make(chan endpointproxy.Header, 4)}
	t.Cleanup(func() { _ = ln.Close() })
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
				u.headers <- h
				_, _ = io.Copy(c, c)
			}()
		}
	}()
	return u
}

// dial reaches the fake upstream whatever node address is asked for, and
// records the address.
func (u *upstream) dial(asked *atomic.Value) EndpointDialFunc {
	return func(ctx context.Context, addr netip.Addr) (net.Conn, error) {
		asked.Store(addr)
		d := tls.Dialer{Config: &tls.Config{RootCAs: u.pool, ServerName: "127.0.0.1", MinVersion: tls.VersionTLS13}}
		return d.DialContext(ctx, "tcp", u.ln.Addr().String())
	}
}

func selfSigned(t *testing.T) (tls.Certificate, *x509.CertPool) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "gate"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		IPAddresses: []net.IP{net.ParseIP("127.0.0.1")},
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IsCA:        true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	require.NoError(t, err)
	leaf, err := x509.ParseCertificate(der)
	require.NoError(t, err)
	pool := x509.NewCertPool()
	pool.AddCert(leaf)
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key, Leaf: leaf}, pool
}

type proxyHarness struct {
	p      *EndpointProxy
	addr   string
	eni    *eniFacts
	src    atomic.Value // netip.Addr the guest's socket reads as
	skew   atomic.Int64 // nanoseconds added to the proxy's clock
	egress atomic.Bool
	dialed atomic.Value
	groups chan []string
}

// newProxyHarness serves one tap over loopback. The accepted socket's loopback
// addresses read as the guest address and epTestEntry, which TPROXY would deliver.
func newProxyHarness(t *testing.T, dial func(h *proxyHarness) EndpointDialFunc) *proxyHarness {
	t.Helper()
	h := &proxyHarness{
		eni: &eniFacts{
			eniID: "eni-0a1b2c3d", accountID: epTestAccount, vpcID: epTestVPC, subnetID: epTestSubnet,
			privateIP: epTestGuest.String(), securityGroupIDs: []string{"sg-0a1b2c3d"},
		},
		groups: make(chan []string, 8),
	}
	h.src.Store(epTestGuest)
	h.egress.Store(true)
	p, err := NewEndpointProxy(EndpointProxyConfig{
		Port: 8473,
		Egress: func(_ context.Context, accountID string, groups []string, dst netip.AddrPort) (bool, error) {
			assert.Equal(t, epTestAccount, accountID)
			assert.Equal(t, epTestEntry, dst)
			h.groups <- groups
			return h.egress.Load(), nil
		},
		Dial:   dial(h),
		MaxAge: time.Minute,
	})
	require.NoError(t, err)
	p.refresh = func(context.Context, *eniFacts) (*eniFacts, error) { return h.eni, nil }
	p.now = func() time.Time { return time.Now().Add(time.Duration(h.skew.Load())) }
	p.connAddrs = func(c net.Conn) (netip.AddrPort, netip.AddrPort, error) {
		r, ok := c.RemoteAddr().(*net.TCPAddr)
		if !ok {
			return netip.AddrPort{}, netip.AddrPort{}, errors.New("not tcp")
		}
		src, _ := h.src.Load().(netip.Addr)
		return netip.AddrPortFrom(src, uint16(r.Port)), epTestEntry, nil
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	h.addr = ln.Addr().String()
	h.p = p
	served := make(chan struct{})
	go func() {
		defer close(served)
		_ = p.serve(t.Context(), newLimitListener(ln, endpointProxyConnsPerTap), h.eni)
	}()
	t.Cleanup(func() {
		_ = ln.Close()
		<-served
		p.shutdown()
	})
	return h
}

func noDial(t *testing.T) func(*proxyHarness) EndpointDialFunc {
	return func(h *proxyHarness) EndpointDialFunc {
		return func(_ context.Context, addr netip.Addr) (net.Conn, error) {
			h.dialed.Store(addr)
			t.Errorf("a refused connection must never be dialled, dialled %s", addr)
			return nil, errors.New("refused")
		}
	}
}

// expectReset asserts the proxy answered the guest with an RST.
func expectReset(t *testing.T, addr string) {
	t.Helper()
	// Over loopback the RST can land before connect returns.
	c, err := net.Dial("tcp", addr)
	if err == nil {
		defer c.Close()
		require.NoError(t, c.SetReadDeadline(time.Now().Add(5*time.Second)))
		_, err = c.Read(make([]byte, 1))
	}
	require.Error(t, err)
	assert.ErrorIs(t, err, syscall.ECONNRESET, "a refused endpoint connection must be reset, not closed or left hanging")
}

func TestEndpointProxy_RefusedConnectionsAreReset(t *testing.T) {
	tests := []struct {
		name    string
		prepare func(h *proxyHarness)
	}{
		{"no view published yet", func(*proxyHarness) {}},
		{"destination is not a prefix-list entry", func(h *proxyHarness) {
			v := testView()
			v.Entries = []netip.AddrPort{epTestOther}
			h.p.Publish(v)
		}},
		{"subnet routes to no endpoint", func(h *proxyHarness) {
			v := testView()
			v.Endpoints = map[string]projection.GatewayEndpoint{"subnet-ffffffff": testEndpoint()}
			h.p.Publish(v)
		}},
		{"endpoint of another VPC", func(h *proxyHarness) {
			v := testView()
			ep := testEndpoint()
			ep.VpcID = "vpc-ffffffffff"
			v.Endpoints = map[string]projection.GatewayEndpoint{epTestSubnet: ep}
			h.p.Publish(v)
		}},
		{"security groups deny the egress", func(h *proxyHarness) {
			h.p.Publish(testView())
			h.egress.Store(false)
		}},
		{"source is not the interface's address", func(h *proxyHarness) {
			h.p.Publish(testView())
			h.src.Store(netip.MustParseAddr("10.0.1.6"))
		}},
		{"view older than its max age", func(h *proxyHarness) {
			h.p.Publish(testView())
			h.skew.Store(int64(2 * time.Minute))
		}},
		{"view withdrawn after a failed pass", func(h *proxyHarness) {
			h.p.Publish(testView())
			h.p.Invalidate()
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newProxyHarness(t, noDial(t))
			tt.prepare(h)
			expectReset(t, h.addr)
			assert.Eventually(t, func() bool { return h.p.Sessions() == 0 }, 5*time.Second, 10*time.Millisecond)
		})
	}
}

func TestEndpointProxy_ChannelFailureResetsWithoutFallback(t *testing.T) {
	var calls atomic.Int32
	h := newProxyHarness(t, func(*proxyHarness) EndpointDialFunc {
		return func(_ context.Context, addr netip.Addr) (net.Conn, error) {
			calls.Add(1)
			assert.Equal(t, epTestEntry.Addr(), addr, "the channel goes to the node the guest dialled")
			return nil, errors.New("handshake refused")
		}
	})
	h.p.Publish(testView())

	expectReset(t, h.addr)
	assert.Equal(t, int32(1), calls.Load(), "one dial of the endpoint listener, and nothing else")
}

func TestEndpointProxy_AdmittedConnectionCarriesTheHeaderAndSplices(t *testing.T) {
	up := startUpstream(t)
	h := newProxyHarness(t, func(h *proxyHarness) EndpointDialFunc { return up.dial(&h.dialed) })
	h.p.Publish(testView())

	c, err := net.Dial("tcp", h.addr)
	require.NoError(t, err)
	defer c.Close()
	require.NoError(t, c.SetDeadline(time.Now().Add(5*time.Second)))

	select {
	case hdr := <-up.headers:
		local, ok := c.LocalAddr().(*net.TCPAddr)
		require.True(t, ok)
		assert.Equal(t, endpointproxy.Header{
			Version:     endpointproxy.ContractVersion1,
			Source:      netip.AddrPortFrom(epTestGuest, uint16(local.Port)),
			Destination: epTestEntry,
			VpceID:      epTestVpce, VpcID: epTestVPC, AccountID: epTestAccount,
		}, hdr)
	case <-time.After(5 * time.Second):
		t.Fatal("the upstream never received a PROXY header")
	}
	assert.Equal(t, epTestEntry.Addr(), h.dialed.Load())
	assert.Equal(t, []string{"sg-0a1b2c3d"}, <-h.groups, "the ENI's own groups decide the egress")

	_, err = c.Write([]byte("client hello"))
	require.NoError(t, err)
	got := make([]byte, len("client hello"))
	_, err = io.ReadFull(c, got)
	require.NoError(t, err)
	assert.Equal(t, "client hello", string(got), "bytes after the header pass both ways unchanged")
	assert.Eventually(t, func() bool { return h.p.Sessions() == 1 }, 5*time.Second, 10*time.Millisecond)
}

func TestEndpointProxy_SecurityGroupChangeAppliesToTheNextConnection(t *testing.T) {
	up := startUpstream(t)
	h := newProxyHarness(t, func(h *proxyHarness) EndpointDialFunc { return up.dial(&h.dialed) })
	h.p.Publish(testView())

	first, err := net.Dial("tcp", h.addr)
	require.NoError(t, err)
	defer first.Close()
	select {
	case <-up.headers:
	case <-time.After(5 * time.Second):
		t.Fatal("first connection was not proxied")
	}

	h.egress.Store(false)
	expectReset(t, h.addr)
	assert.Equal(t, 1, h.p.Sessions(), "an established session continues, as conntrack lets it on AWS")
}

func TestEndpointProxy_PublishClosesSessionsTheViewNoLongerAdmits(t *testing.T) {
	tests := []struct {
		name  string
		view  func() EndpointView
		keeps bool
	}{
		{"same endpoint", testView, true},
		{"endpoint removed from the subnet", func() EndpointView {
			v := testView()
			v.Endpoints = map[string]projection.GatewayEndpoint{}
			return v
		}, false},
		{"subnet moved to another endpoint", func() EndpointView {
			v := testView()
			ep := testEndpoint()
			ep.ID = "vpce-0fedcba9876543210"
			v.Endpoints = map[string]projection.GatewayEndpoint{epTestSubnet: ep}
			return v
		}, false},
		{"entry left the prefix list", func() EndpointView {
			v := testView()
			v.Entries = []netip.AddrPort{epTestOther}
			return v
		}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			up := startUpstream(t)
			h := newProxyHarness(t, func(h *proxyHarness) EndpointDialFunc { return up.dial(&h.dialed) })
			h.p.Publish(testView())
			c, err := net.Dial("tcp", h.addr)
			require.NoError(t, err)
			defer c.Close()
			<-up.headers
			require.Eventually(t, func() bool { return h.p.Sessions() == 1 }, 5*time.Second, 10*time.Millisecond)

			closed := h.p.Publish(tt.view())
			if tt.keeps {
				assert.Zero(t, closed)
				assert.Equal(t, 1, h.p.Sessions())
				return
			}
			assert.Equal(t, 1, closed)
			assert.Eventually(t, func() bool { return h.p.Sessions() == 0 }, 5*time.Second, 10*time.Millisecond)
			require.NoError(t, c.SetReadDeadline(time.Now().Add(5*time.Second)))
			_, err = c.Read(make([]byte, 1))
			assert.ErrorIs(t, err, syscall.ECONNRESET, "a withdrawn session is reset")
		})
	}
}

// A connection counts from accept until its handler has closed both sockets,
// so a dial still in progress when its endpoint is withdrawn holds a rollback.
func TestEndpointProxy_PendingDialCountsUntilItFinishes(t *testing.T) {
	up := startUpstream(t)
	release := make(chan struct{})
	h := newProxyHarness(t, func(h *proxyHarness) EndpointDialFunc {
		dial := up.dial(&h.dialed)
		return func(ctx context.Context, addr netip.Addr) (net.Conn, error) {
			<-release
			return dial(ctx, addr)
		}
	})
	h.p.Publish(testView())
	c, err := net.Dial("tcp", h.addr)
	require.NoError(t, err)
	defer c.Close()
	require.Eventually(t, func() bool { return h.p.Sessions() == 1 }, 5*time.Second, 10*time.Millisecond, "a dial in progress is not counted")

	v := testView()
	v.Endpoints = map[string]projection.GatewayEndpoint{}
	assert.Zero(t, h.p.Publish(v), "no session was registered to close")
	assert.Equal(t, 1, h.p.Sessions(), "a withdrawn dial still in progress reads as idle")

	close(release)
	require.NoError(t, c.SetReadDeadline(time.Now().Add(5*time.Second)))
	_, err = c.Read(make([]byte, 1))
	assert.ErrorIs(t, err, syscall.ECONNRESET, "a dial the view withdrew is reset")
	assert.Eventually(t, func() bool { return h.p.Sessions() == 0 }, 5*time.Second, 10*time.Millisecond)
}

func TestEndpointProxy_ShutdownResetsSessionsAndRefusesNewOnes(t *testing.T) {
	up := startUpstream(t)
	h := newProxyHarness(t, func(h *proxyHarness) EndpointDialFunc { return up.dial(&h.dialed) })
	h.p.Publish(testView())
	c, err := net.Dial("tcp", h.addr)
	require.NoError(t, err)
	defer c.Close()
	<-up.headers
	require.Eventually(t, func() bool { return h.p.Sessions() == 1 }, 5*time.Second, 10*time.Millisecond)

	h.p.shutdown()

	assert.Zero(t, h.p.Sessions())
	require.NoError(t, c.SetReadDeadline(time.Now().Add(5*time.Second)))
	_, err = c.Read(make([]byte, 1))
	require.ErrorIs(t, err, syscall.ECONNRESET)
	h.p.Publish(testView())
	expectReset(t, h.addr)
}

func TestTapResponder_StoppingATapResetsItsEndpointSessions(t *testing.T) {
	const eniID, endpoint = "eni-0a1b2c3d", "ime-0a1b2c3d"
	up := startUpstream(t)
	h := newProxyHarness(t, func(h *proxyHarness) EndpointDialFunc { return up.dial(&h.dialed) })
	var addrs sync.Map
	h.p.listen = loopbackTapListen(&addrs)
	h.p.Publish(testView())
	m := newTapResponderManager(nil, staticResolve(map[string]*eniFacts{eniID: h.eni}), loopbackTapListen(&sync.Map{}))
	m.enableEndpointProxy(h.p)
	m.ifindex = func(string) (int, error) { return 0, nil }

	m.reconcile(t.Context(), map[string]string{eniID: endpoint})
	raw, ok := addrs.Load(endpoint)
	require.True(t, ok, "reconcile binds the tap's proxy listener")
	addr, ok := raw.(string)
	require.True(t, ok)
	c, err := net.Dial("tcp", addr)
	require.NoError(t, err)
	defer c.Close()
	<-up.headers
	require.Eventually(t, func() bool { return h.p.Sessions() == 1 }, 5*time.Second, 10*time.Millisecond)

	m.reconcile(t.Context(), map[string]string{})

	assert.Eventually(t, func() bool { return h.p.Sessions() == 0 }, 5*time.Second, 10*time.Millisecond, "a gone tap keeps no session")
	_, err = net.DialTimeout("tcp", addr, time.Second)
	assert.Error(t, err, "a gone tap keeps no proxy listener")
}

// The binder's socket options need CAP_NET_ADMIN; a refusal here is what a
// vpcd without the capability would hit on every tap.
func TestEndpointProxyBinder_AcceptsWithTheDialledLocalAddress(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("IP_TRANSPARENT needs CAP_NET_ADMIN; run as root")
	}
	ln, err := endpointProxyBinder(0)(t.Context(), "lo")
	require.NoError(t, err)
	defer ln.Close()
	tcp, ok := ln.Addr().(*net.TCPAddr)
	require.True(t, ok)

	c, err := net.Dial("tcp4", net.JoinHostPort("127.0.0.1", strconv.Itoa(tcp.Port)))
	require.NoError(t, err)
	defer c.Close()
	got, err := ln.Accept()
	require.NoError(t, err)
	defer got.Close()
	_, dst, err := socketAddrs(got)
	require.NoError(t, err)
	assert.Equal(t, netip.AddrPortFrom(netip.MustParseAddr("127.0.0.1"), uint16(tcp.Port)), dst)
}
