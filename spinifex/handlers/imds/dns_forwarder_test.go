package handlers_imds

import (
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// testDNSQuery is a minimal well-formed DNS query header (ID 0xABCD, RD set,
// QDCOUNT 1) plus a question for "a." IN A — enough for wire-level relay checks.
var testDNSQuery = []byte{
	0xAB, 0xCD, 0x01, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
	0x01, 'a', 0x00, 0x00, 0x01, 0x00, 0x01,
}

// fakeUDPUpstream runs a UDP backend answering with respond(query); a nil
// response means stay silent (drives the forwarder's timeout/failover path).
func fakeUDPUpstream(t *testing.T, respond func(q []byte) []byte) string {
	t.Helper()
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = pc.Close() })
	go func() {
		buf := make([]byte, maxDNSUDPSize)
		for {
			n, addr, err := pc.ReadFrom(buf)
			if err != nil {
				return
			}
			if resp := respond(buf[:n]); resp != nil {
				_, _ = pc.WriteTo(resp, addr)
			}
		}
	}()
	return pc.LocalAddr().String()
}

// markedResponse flips the QR bit and appends a marker byte, so the test can
// tell which backend answered and that bytes passed through unmodified.
func markedResponse(marker byte) func(q []byte) []byte {
	return func(q []byte) []byte {
		resp := make([]byte, len(q), len(q)+1)
		copy(resp, q)
		resp[2] |= 0x80
		return append(resp, marker)
	}
}

func silent(_ []byte) []byte { return nil }

func TestDNSForwarder_UDPRelaysToBackend(t *testing.T) {
	backend := fakeUDPUpstream(t, markedResponse('X'))
	f := newDNSForwarder([]string{backend})

	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	require.NoError(t, err)
	go f.serveUDP(pc)
	defer func() { _ = pc.Close() }()

	resp := udpQuery(t, pc.LocalAddr().String(), testDNSQuery)
	require.Len(t, resp, len(testDNSQuery)+1, "relay must pass response bytes through unmodified")
	assert.Equal(t, byte('X'), resp[len(resp)-1])
	assert.Equal(t, testDNSQuery[0:2], resp[0:2], "DNS ID must round-trip")
}

func TestDNSForwarder_UDPFailsOverOnTimeout(t *testing.T) {
	dead := fakeUDPUpstream(t, silent)
	live := fakeUDPUpstream(t, markedResponse('Y'))
	f := newDNSForwarder([]string{dead, live})
	f.timeout = 100 * time.Millisecond

	resp, err := f.exchangeUDP(testDNSQuery)
	require.NoError(t, err, "second backend must answer after the first times out")
	assert.Equal(t, byte('Y'), resp[len(resp)-1], "answer must come from the live backend")
}

func TestDNSForwarder_UDPServfailWhenAllBackendsDead(t *testing.T) {
	dead := fakeUDPUpstream(t, silent)
	f := newDNSForwarder([]string{dead})
	f.timeout = 100 * time.Millisecond

	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	require.NoError(t, err)
	go f.serveUDP(pc)
	defer func() { _ = pc.Close() }()

	resp := udpQuery(t, pc.LocalAddr().String(), testDNSQuery)
	require.GreaterOrEqual(t, len(resp), 12)
	assert.Equal(t, testDNSQuery[0:2], resp[0:2], "SERVFAIL must echo the query ID")
	assert.NotZero(t, resp[2]&0x80, "QR bit must be set")
	assert.Equal(t, byte(2), resp[3]&0x0F, "RCODE must be SERVFAIL")
}

func TestDNSForwarder_TCPProxiesFramedSession(t *testing.T) {
	// Echo backend: framed request bytes come back verbatim, proving the proxy
	// relays the length-prefixed stream untouched in both directions.
	backendLn, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = backendLn.Close() })
	go func() {
		for {
			c, err := backendLn.Accept()
			if err != nil {
				return
			}
			go func() { _, _ = io.Copy(c, c); _ = c.Close() }()
		}
	}()

	f := newDNSForwarder([]string{backendLn.Addr().String()})
	shimLn, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	go f.serveTCP(shimLn)
	defer func() { _ = shimLn.Close() }()

	conn, err := net.Dial("tcp", shimLn.Addr().String())
	require.NoError(t, err)
	defer func() { _ = conn.Close() }()
	require.NoError(t, conn.SetDeadline(time.Now().Add(2*time.Second)))

	framed := make([]byte, 2+len(testDNSQuery))
	binary.BigEndian.PutUint16(framed, uint16(len(testDNSQuery)))
	copy(framed[2:], testDNSQuery)
	_, err = conn.Write(framed)
	require.NoError(t, err)

	got := make([]byte, len(framed))
	_, err = io.ReadFull(conn, got)
	require.NoError(t, err)
	assert.Equal(t, framed, got, "TCP session must relay bytes both ways")
}

func TestServfail_RejectsRunt(t *testing.T) {
	assert.Nil(t, servfail([]byte{0x01, 0x02}), "a message without a DNS header has nothing to answer")
}

// ----- H3: backend health memory ----------------------------------------

// After a backend fails, it is deprioritised for the cooldown window so the next
// query hits a healthy backend first instead of eating the dead one's timeout.
func TestDNSForwarder_DownBackendDeprioritisedAfterFailure(t *testing.T) {
	dead := fakeUDPUpstream(t, silent)
	live := fakeUDPUpstream(t, markedResponse('Y'))
	f := newDNSForwarder([]string{dead, live})
	f.timeout = 100 * time.Millisecond
	base := time.Now()
	f.now = func() time.Time { return base }

	// First query pays the dead backend's timeout, then answers via live.
	resp, err := f.exchangeUDP(testDNSQuery)
	require.NoError(t, err)
	assert.Equal(t, byte('Y'), resp[len(resp)-1])

	require.False(t, f.backends[0].healthy(base), "failed backend must be marked down")
	require.True(t, f.backends[1].healthy(base), "responding backend must stay healthy")
	order := f.ordered(base)
	assert.Equal(t, live, order[0].addr, "healthy backend must be tried first")

	// The second query must not eat the dead backend's timeout again.
	start := time.Now()
	_, err = f.exchangeUDP(testDNSQuery)
	require.NoError(t, err)
	assert.Less(t, time.Since(start), f.timeout, "down backend must be skipped, not timed out on")
}

// A recovered backend is marked healthy again once it answers.
func TestDNSForwarder_BackendRecovers(t *testing.T) {
	b := &dnsBackend{addr: "10.0.0.1:53"}
	now := time.Now()
	b.markDown(now, 10*time.Second)
	assert.False(t, b.healthy(now), "backend is down during cooldown")
	assert.True(t, b.healthy(now.Add(11*time.Second)), "backend recovers after cooldown")
	b.markUp()
	assert.True(t, b.healthy(now), "an explicit markUp clears the cooldown immediately")
}

// ----- H2: per-tap rate limit + concurrency cap -------------------------

// The token bucket admits a burst then sheds, and refills over elapsed time.
func TestTokenBucket_ShedsBurstThenRefills(t *testing.T) {
	now := time.Now()
	clock := func() time.Time { return now }
	tb := newTokenBucket(100, 3, clock)

	for i := range 3 {
		assert.True(t, tb.allow(), "burst token %d must be admitted", i)
	}
	assert.False(t, tb.allow(), "query beyond the burst must be shed while the clock is frozen")

	now = now.Add(50 * time.Millisecond) // 100/s * 0.05s = 5 tokens refilled
	assert.True(t, tb.allow(), "a query must be admitted after the bucket refills")
}

// A non-positive rate disables limiting entirely.
func TestTokenBucket_ZeroRateAllowsAll(t *testing.T) {
	tb := newTokenBucket(0, 0, time.Now)
	for range 1000 {
		require.True(t, tb.allow())
	}
}

// A flood on one tap is shed to the burst size (H2): with the clock frozen so the
// bucket never refills, only `burst` queries reach the backend and the rest drop.
func TestDNSForwarder_UDPFloodShedToBurst(t *testing.T) {
	var received atomic.Int32
	backend := fakeUDPUpstream(t, func(q []byte) []byte {
		received.Add(1)
		return markedResponse('F')(q)
	})
	f := newDNSForwarder([]string{backend})
	base := time.Now()
	f.now = func() time.Time { return base } // freeze: no refill
	f.rate, f.burst = 100, 3

	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	require.NoError(t, err)
	go f.serveUDP(pc)
	defer func() { _ = pc.Close() }()

	client, err := net.Dial("udp", pc.LocalAddr().String())
	require.NoError(t, err)
	defer func() { _ = client.Close() }()

	const flood = 8
	for range flood {
		_, err := client.Write(testDNSQuery)
		require.NoError(t, err)
	}

	// Count answers: only the burst allotment is relayed, the rest are shed.
	answered := 0
	buf := make([]byte, maxDNSUDPSize)
	for {
		_ = client.SetReadDeadline(time.Now().Add(300 * time.Millisecond))
		if _, err := client.Read(buf); err != nil {
			break
		}
		answered++
	}
	assert.Equal(t, 3, answered, "flood must be shed to the burst size")
	assert.Equal(t, int32(3), received.Load(), "backend must see only the admitted queries")
}

// ----- manager lifecycle -------------------------------------------------

// dnsSocketPair records the loopback shim sockets bound for one endpoint.
type dnsSocketPair struct {
	pc net.PacketConn
	ln net.Listener
}

func (p *dnsSocketPair) closed() bool {
	_, err := p.pc.WriteTo(nil, p.pc.LocalAddr())
	return errors.Is(err, net.ErrClosed)
}

// loopbackDNSListen returns a dnsListenFunc backed by real loopback sockets,
// recording each endpoint's pair so tests can drive and inspect them.
func loopbackDNSListen(pairs *sync.Map) dnsListenFunc {
	return func(_ context.Context, endpoint string) (net.PacketConn, net.Listener, error) {
		pc, err := net.ListenPacket("udp", "127.0.0.1:0")
		if err != nil {
			return nil, nil, err
		}
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			_ = pc.Close()
			return nil, nil, err
		}
		pairs.Store(endpoint, &dnsSocketPair{pc: pc, ln: ln})
		return pc, ln, nil
	}
}

// The DNS shim follows the tap lifecycle: reconcile binds the shim with the
// responder, serves queries end-to-end through the forwarder, and closes the
// sockets when the tap goes away.
func TestTapResponder_DNSFollowsTapLifecycle(t *testing.T) {
	backend := fakeUDPUpstream(t, markedResponse('Z'))
	table := map[string]*eniFacts{"eni-aaa11111": {eniID: "eni-aaa11111", instanceID: "i-aaaa1111"}}

	var addrs, pairs sync.Map
	m := newTapResponderManager(http.NewServeMux(), staticResolve(table), loopbackTapListen(&addrs))
	m.enableDNS(loopbackDNSListen(&pairs), newDNSForwarder([]string{backend}))
	ctx := context.Background()

	m.reconcile(ctx, map[string]string{"eni-aaa11111": "ime-aaa11111"})
	raw, ok := pairs.Load("ime-aaa11111")
	require.True(t, ok, "reconcile must bind the DNS shim with the responder")
	pair, ok := raw.(*dnsSocketPair)
	require.True(t, ok)

	resp := udpQuery(t, pair.pc.LocalAddr().String(), testDNSQuery)
	assert.Equal(t, byte('Z'), resp[len(resp)-1], "shim must relay guest queries to the backend")

	// Tap gone: the shim sockets close with the responder.
	m.reconcile(ctx, map[string]string{})
	assertNotActive(t, m, "eni-aaa11111")
	assert.True(t, pair.closed(), "DNS shim sockets must close when the tap goes away")
}

// A DNS bind failure must not block IMDS serving, and the shim must come up on
// a later reconcile once the bind succeeds — the retry-next-tick contract.
func TestTapResponder_DNSBindFailureNeverBlocksIMDS(t *testing.T) {
	table := map[string]*eniFacts{"eni-aaa11111": {eniID: "eni-aaa11111", instanceID: "i-aaaa1111"}}

	var addrs, pairs sync.Map
	var failBind bool
	flaky := func(ctx context.Context, endpoint string) (net.PacketConn, net.Listener, error) {
		if failBind {
			return nil, nil, errors.New("bind refused")
		}
		return loopbackDNSListen(&pairs)(ctx, endpoint)
	}

	m := newTapResponderManager(http.NewServeMux(), staticResolve(table), loopbackTapListen(&addrs))
	m.enableDNS(flaky, newDNSForwarder(nil))
	ctx := context.Background()

	failBind = true
	require.NoError(t, m.start(ctx, "eni-aaa11111", "ime-aaa11111"), "DNS bind failure must not fail the responder")
	assertActive(t, m, "eni-aaa11111")
	_, bound := pairs.Load("ime-aaa11111")
	assert.False(t, bound)

	// HTTP still serves despite the failed DNS bind.
	client := http.Client{Timeout: time.Second}
	resp, err := client.Get("http://" + mustAddr(t, &addrs, "ime-aaa11111") + "/")
	require.NoError(t, err, "IMDS must serve with the DNS shim down")
	_ = resp.Body.Close()

	// Next reconcile retries the bind and brings the shim up; a further pass is
	// idempotent (no second bind).
	failBind = false
	m.reconcile(ctx, map[string]string{"eni-aaa11111": "ime-aaa11111"})
	rawFirst, bound := pairs.Load("ime-aaa11111")
	require.True(t, bound, "reconcile must retry the DNS bind")
	m.reconcile(ctx, map[string]string{"eni-aaa11111": "ime-aaa11111"})
	rawSecond, _ := pairs.Load("ime-aaa11111")
	assert.Same(t, rawFirst, rawSecond, "a bound shim must not be rebound")

	m.shutdown()
}

// udpQuery sends one datagram to addr and returns the response.
func udpQuery(t *testing.T, addr string, query []byte) []byte {
	t.Helper()
	conn, err := net.Dial("udp", addr)
	require.NoError(t, err)
	defer func() { _ = conn.Close() }()
	require.NoError(t, conn.SetDeadline(time.Now().Add(2*time.Second)))
	_, err = conn.Write(query)
	require.NoError(t, err)
	buf := make([]byte, maxDNSUDPSize)
	n, err := conn.Read(buf)
	require.NoError(t, err)
	return buf[:n]
}
