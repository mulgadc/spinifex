package handlers_imds

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"sync"
	"time"
)

// maxDNSUDPSize is the largest UDP DNS message the shim relays; EDNS0 clients
// advertise up to 4096, so the relay buffer must not truncate below that.
const maxDNSUDPSize = 4096

// dnsExchangeTimeout bounds one backend attempt (UDP round-trip or TCP dial),
// so failover across every backend stays inside a guest resolver's ~5s retry.
const dnsExchangeTimeout = 2 * time.Second

// dnsTCPSessionTimeout bounds a proxied TCP DNS session end-to-end.
const dnsTCPSessionTimeout = 30 * time.Second

// northstarResolverPort is northstar's unprivileged wildcard listener; the shim
// forwards there rather than the privileged node-WAN :53 bind, so it does not
// depend on that exception surviving.
const northstarResolverPort = "5300"

// dnsBackendCooldown is how long a backend stays deprioritised after a failed
// exchange, so a dead backend[0] stops costing every query a full timeout (H3).
const dnsBackendCooldown = 10 * time.Second

// dnsMaxInFlightPerTap caps concurrent in-flight exchanges on one tap's shim, so
// a flooding or broken guest cannot spawn unbounded goroutines inside vpcd (H2).
const dnsMaxInFlightPerTap = 256

// dnsQueryRatePerTap / dnsQueryBurstPerTap bound a single tap's sustained and
// burst query rate; a legitimate guest never approaches these, a flood is shed.
// The rate mirrors AWS Route 53 Resolver's 10,000 UDP-QPS-per-endpoint-IP quota
// (handlers/dns.DefaultResolverQPSPerIP): the per-tap .253 shim is our
// resolver-endpoint analog, so this is the AWS-parity ceiling.
const (
	dnsQueryRatePerTap  = 10_000
	dnsQueryBurstPerTap = 20_000
)

// dnsListenFunc binds the per-tap DNS shim sockets — UDP and TCP on
// 169.254.169.253:53, scoped to the tap's endpoint device via SO_BINDTODEVICE.
// Swapped in tests to avoid the privileges and address a real endpoint needs.
type dnsListenFunc func(ctx context.Context, endpoint string) (net.PacketConn, net.Listener, error)

// dnsBackend is one northstar target with failure memory: after a failed
// exchange it is marked down until downUntil, so healthy backends are tried
// first and a dead one is skipped rather than timed out on every query (H3).
type dnsBackend struct {
	addr string
	mu   sync.Mutex
	down time.Time // deprioritised until this instant; zero => healthy
}

func (b *dnsBackend) healthy(now time.Time) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return !now.Before(b.down)
}

func (b *dnsBackend) markDown(now time.Time, cooldown time.Duration) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.down = now.Add(cooldown)
}

func (b *dnsBackend) markUp() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.down = time.Time{}
}

// dnsForwarder is the per-tap VPC DNS shim: a wire-level relay from the tap's
// 169.254.169.253:53 to the northstar backends, not a resolver. Queries are
// forwarded plainly; per-VPC identity tagging is reserved for private hosted zones.
type dnsForwarder struct {
	backends    []*dnsBackend // northstar targets, health-tried healthy-first
	timeout     time.Duration
	cooldown    time.Duration // backend down-marking window (H3)
	maxInFlight int           // per-tap concurrency cap (H2)
	rate, burst int           // per-tap token-bucket query rate (H2)
	now         func() time.Time
}

func newDNSForwarder(targets []string) *dnsForwarder {
	backends := make([]*dnsBackend, len(targets))
	for i, t := range targets {
		backends[i] = &dnsBackend{addr: t}
	}
	return &dnsForwarder{
		backends:    backends,
		timeout:     dnsExchangeTimeout,
		cooldown:    dnsBackendCooldown,
		maxInFlight: dnsMaxInFlightPerTap,
		rate:        dnsQueryRatePerTap,
		burst:       dnsQueryBurstPerTap,
		now:         time.Now,
	}
}

// ordered returns the backends healthy-first (input order preserved within each
// group), so a backend in its cooldown window is tried only after every healthy
// one — the fix for backend[0]-down eating a timeout on every query (H3).
func (f *dnsForwarder) ordered(now time.Time) []*dnsBackend {
	healthy := make([]*dnsBackend, 0, len(f.backends))
	down := make([]*dnsBackend, 0, len(f.backends))
	for _, b := range f.backends {
		if b.healthy(now) {
			healthy = append(healthy, b)
		} else {
			down = append(down, b)
		}
	}
	return append(healthy, down...)
}

// serveUDP relays each datagram to the first responsive backend and writes the
// answer back to the querying guest. A per-tap token bucket sheds flood traffic
// and an in-flight semaphore caps concurrent exchanges, so one guest cannot
// exhaust vpcd (H2). Exits when the socket is closed.
func (f *dnsForwarder) serveUDP(pc net.PacketConn) {
	sem := make(chan struct{}, f.maxInFlight)
	limiter := newTokenBucket(f.rate, f.burst, f.now)
	buf := make([]byte, maxDNSUDPSize)
	for {
		n, addr, err := pc.ReadFrom(buf)
		if err != nil {
			if !errors.Is(err, net.ErrClosed) {
				slog.Warn("IMDS: DNS shim UDP read failed", "err", err)
			}
			return
		}
		if !limiter.allow() {
			slog.Debug("IMDS: DNS shim UDP query rate-limited", "client", addr.String())
			continue
		}
		select {
		case sem <- struct{}{}:
		default:
			slog.Debug("IMDS: DNS shim UDP in-flight cap reached, dropping", "client", addr.String())
			continue
		}
		query := make([]byte, n)
		copy(query, buf[:n])
		go func() {
			defer func() { <-sem }()
			resp, err := f.exchangeUDP(query)
			if err != nil {
				slog.Debug("IMDS: DNS shim exchange failed on all backends", "err", err)
				resp = servfail(query)
				if resp == nil {
					return
				}
			}
			if _, err := pc.WriteTo(resp, addr); err != nil && !errors.Is(err, net.ErrClosed) {
				slog.Debug("IMDS: DNS shim UDP reply write failed", "client", addr.String(), "err", err)
			}
		}()
	}
}

// exchangeUDP relays one query to the backends healthy-first, returning the first
// answer received within the per-backend timeout. A failing backend is marked
// down for the cooldown window; a responding one is marked healthy again.
func (f *dnsForwarder) exchangeUDP(query []byte) ([]byte, error) {
	var lastErr error
	for _, b := range f.ordered(f.now()) {
		resp, err := f.exchangeUDPOnce(b.addr, query)
		if err != nil {
			b.markDown(f.now(), f.cooldown)
			lastErr = err
			continue
		}
		b.markUp()
		return resp, nil
	}
	if lastErr == nil {
		lastErr = errors.New("no DNS backends configured")
	}
	return nil, lastErr
}

func (f *dnsForwarder) exchangeUDPOnce(target string, query []byte) ([]byte, error) {
	conn, err := net.DialTimeout("udp", target, f.timeout)
	if err != nil {
		return nil, err
	}
	defer func() { _ = conn.Close() }()
	if err := conn.SetDeadline(time.Now().Add(f.timeout)); err != nil {
		return nil, err
	}
	if _, err := conn.Write(query); err != nil {
		return nil, err
	}
	buf := make([]byte, maxDNSUDPSize)
	n, err := conn.Read(buf)
	if err != nil {
		return nil, err
	}
	return buf[:n], nil
}

// serveTCP accepts guest TCP DNS sessions (truncated-response retries) and
// proxies each to the first dialable backend. A per-tap token bucket and
// in-flight semaphore bound accepts the same way serveUDP bounds datagrams (H2).
// Exits when the listener is closed.
func (f *dnsForwarder) serveTCP(ln net.Listener) {
	sem := make(chan struct{}, f.maxInFlight)
	limiter := newTokenBucket(f.rate, f.burst, f.now)
	for {
		client, err := ln.Accept()
		if err != nil {
			if !errors.Is(err, net.ErrClosed) {
				slog.Warn("IMDS: DNS shim TCP accept failed", "err", err)
			}
			return
		}
		if !limiter.allow() {
			slog.Debug("IMDS: DNS shim TCP session rate-limited", "client", client.RemoteAddr().String())
			_ = client.Close()
			continue
		}
		select {
		case sem <- struct{}{}:
		default:
			slog.Debug("IMDS: DNS shim TCP in-flight cap reached, dropping", "client", client.RemoteAddr().String())
			_ = client.Close()
			continue
		}
		go func() {
			defer func() { <-sem }()
			f.proxyTCP(client)
		}()
	}
}

// proxyTCP relays one TCP DNS session byte-for-byte (length-prefixed framing
// passes through untouched), bounded by the session deadline. Backends are
// dialed healthy-first with the same down-marking as the UDP path (H3).
func (f *dnsForwarder) proxyTCP(client net.Conn) {
	defer func() { _ = client.Close() }()
	_ = client.SetDeadline(time.Now().Add(dnsTCPSessionTimeout))

	var upstream net.Conn
	var lastErr error
	for _, b := range f.ordered(f.now()) {
		upstream, lastErr = net.DialTimeout("tcp", b.addr, f.timeout)
		if lastErr != nil {
			b.markDown(f.now(), f.cooldown)
			continue
		}
		b.markUp()
		break
	}
	if upstream == nil {
		slog.Debug("IMDS: DNS shim TCP dial failed on all backends", "err", lastErr)
		return
	}
	defer func() { _ = upstream.Close() }()
	_ = upstream.SetDeadline(time.Now().Add(dnsTCPSessionTimeout))

	// Closing both conns on first-direction exit (via the defers) unblocks the
	// other copy, so neither goroutine outlives the session.
	done := make(chan struct{}, 2)
	go func() {
		_, _ = io.Copy(upstream, client)
		done <- struct{}{}
	}()
	go func() {
		_, _ = io.Copy(client, upstream)
		done <- struct{}{}
	}()
	<-done
}

// tokenBucket is a per-tap query rate limiter (H2). It is not shared across taps
// — each serveUDP/serveTCP invocation owns one — so the limit is inherently
// per-tap. now is injected so tests drive refill deterministically.
type tokenBucket struct {
	mu     sync.Mutex
	tokens float64
	burst  float64
	rate   float64
	last   time.Time
	now    func() time.Time
}

func newTokenBucket(rate, burst int, now func() time.Time) *tokenBucket {
	return &tokenBucket{
		tokens: float64(burst),
		burst:  float64(burst),
		rate:   float64(rate),
		last:   now(),
		now:    now,
	}
}

// allow refills by elapsed time and consumes one token, reporting whether the
// query may proceed. A non-positive rate disables limiting (always allow).
func (tb *tokenBucket) allow() bool {
	if tb.rate <= 0 {
		return true
	}
	tb.mu.Lock()
	defer tb.mu.Unlock()
	now := tb.now()
	tb.tokens += now.Sub(tb.last).Seconds() * tb.rate
	if tb.tokens > tb.burst {
		tb.tokens = tb.burst
	}
	tb.last = now
	if tb.tokens >= 1 {
		tb.tokens--
		return true
	}
	return false
}

// bindTapDNS opens the DNS shim sockets — UDP and TCP 169.254.169.253:53 on the
// tap's endpoint via SO_BINDTODEVICE. Like the .254 HTTP bind, no netns is
// involved: the endpoint owns .253, and the per-tap demux flow already steers
// guest queries here, so binding it lights up the reserved co-tenant slot.
func bindTapDNS(ctx context.Context, endpoint string) (net.PacketConn, net.Listener, error) {
	lc := net.ListenConfig{Control: bindToDeviceControl(endpoint)}
	addr := net.JoinHostPort(VPCDNSServerIP, "53")
	pc, err := lc.ListenPacket(ctx, "udp4", addr)
	if err != nil {
		return nil, nil, err
	}
	ln, err := lc.Listen(ctx, "tcp4", addr)
	if err != nil {
		_ = pc.Close()
		return nil, nil, err
	}
	return pc, ln, nil
}

// servfail rewrites a query header in place into a minimal SERVFAIL response, so
// a guest whose backends are all unreachable fails fast instead of waiting out
// its resolver timeout. Returns nil for runts that carry no DNS header.
func servfail(query []byte) []byte {
	if len(query) < 12 {
		return nil
	}
	resp := make([]byte, len(query))
	copy(resp, query)
	resp[2] = (query[2] & 0x79) | 0x80 // QR=1, keep Opcode+RD, clear AA/TC
	resp[3] = 0x82                     // RA=1, RCODE=SERVFAIL
	return resp
}
