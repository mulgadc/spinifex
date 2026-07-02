package handlers_imds

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
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

// dnsListenFunc binds the per-tap DNS shim sockets — UDP and TCP on
// 169.254.169.253:53, scoped to the tap's endpoint device via SO_BINDTODEVICE.
// Swapped in tests to avoid the privileges and address a real endpoint needs.
type dnsListenFunc func(ctx context.Context, endpoint string) (net.PacketConn, net.Listener, error)

// dnsForwarder is the per-tap VPC DNS shim: a wire-level relay from the tap's
// 169.254.169.253:53 to the northstar backends, not a resolver. Queries are
// forwarded plainly; per-VPC identity tagging is reserved for private hosted zones.
type dnsForwarder struct {
	targets []string // northstar backends (host:port), tried in order
	timeout time.Duration
}

func newDNSForwarder(targets []string) *dnsForwarder {
	return &dnsForwarder{targets: targets, timeout: dnsExchangeTimeout}
}

// serveUDP relays each datagram to the first responsive backend and writes the
// answer back to the querying guest. Exits when the socket is closed.
func (f *dnsForwarder) serveUDP(pc net.PacketConn) {
	buf := make([]byte, maxDNSUDPSize)
	for {
		n, addr, err := pc.ReadFrom(buf)
		if err != nil {
			if !errors.Is(err, net.ErrClosed) {
				slog.Warn("IMDS: DNS shim UDP read failed", "err", err)
			}
			return
		}
		query := make([]byte, n)
		copy(query, buf[:n])
		go func() {
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

// exchangeUDP relays one query to the backends in order, returning the first
// answer received within the per-backend timeout.
func (f *dnsForwarder) exchangeUDP(query []byte) ([]byte, error) {
	var lastErr error
	for _, target := range f.targets {
		resp, err := f.exchangeUDPOnce(target, query)
		if err != nil {
			lastErr = err
			continue
		}
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
// proxies each to the first dialable backend. Exits when the listener is closed.
func (f *dnsForwarder) serveTCP(ln net.Listener) {
	for {
		client, err := ln.Accept()
		if err != nil {
			if !errors.Is(err, net.ErrClosed) {
				slog.Warn("IMDS: DNS shim TCP accept failed", "err", err)
			}
			return
		}
		go f.proxyTCP(client)
	}
}

// proxyTCP relays one TCP DNS session byte-for-byte (length-prefixed framing
// passes through untouched), bounded by the session deadline.
func (f *dnsForwarder) proxyTCP(client net.Conn) {
	defer func() { _ = client.Close() }()
	_ = client.SetDeadline(time.Now().Add(dnsTCPSessionTimeout))

	var upstream net.Conn
	var lastErr error
	for _, target := range f.targets {
		upstream, lastErr = net.DialTimeout("tcp", target, f.timeout)
		if lastErr == nil {
			break
		}
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
