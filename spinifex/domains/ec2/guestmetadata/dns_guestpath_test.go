package guestmetadata

import (
	"context"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"github.com/miekg/dns"
	nsconfig "github.com/mulgadc/northstar/pkg/config"
	nsserver "github.com/mulgadc/northstar/pkg/server"
	"github.com/stretchr/testify/require"
)

// The guest path end to end, in one process: a guest-style query goes to the
// shim, the shim relays to a real northstar server, and northstar forwards to
// upstream resolvers the test controls.

// guestAttempt is a guest resolver's per-attempt wait (glibc and Go both use 5 s).
const guestAttempt = 5 * time.Second

type countingUpstream struct {
	addr    string
	queries atomic.Int32
}

// startAnsweringUpstream is a working recursive resolver: every A query gets 192.0.2.10.
func startAnsweringUpstream(t *testing.T) *countingUpstream {
	t.Helper()
	return startUpstreamWith(t, func(w dns.ResponseWriter, r *dns.Msg) {
		m := new(dns.Msg)
		m.SetReply(r)
		m.RecursionAvailable = true
		rr, _ := dns.NewRR(r.Question[0].Name + " 60 IN A 192.0.2.10")
		m.Answer = []dns.RR{rr}
		_ = w.WriteMsg(m)
	})
}

func startRcodeUpstream(t *testing.T, code int) *countingUpstream {
	t.Helper()
	return startUpstreamWith(t, func(w dns.ResponseWriter, r *dns.Msg) {
		m := new(dns.Msg)
		m.SetRcode(r, code)
		_ = w.WriteMsg(m)
	})
}

func startUpstreamWith(t *testing.T, h func(dns.ResponseWriter, *dns.Msg)) *countingUpstream {
	t.Helper()
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	require.NoError(t, err)
	u := &countingUpstream{addr: pc.LocalAddr().String()}
	srv := &dns.Server{PacketConn: pc, Handler: dns.HandlerFunc(func(w dns.ResponseWriter, r *dns.Msg) {
		u.queries.Add(1)
		h(w, r)
	})}
	go func() { _ = srv.ActivateAndServe() }()
	t.Cleanup(func() { _ = srv.Shutdown() })
	return u
}

// startSilentUpstream reads queries and never answers, like a dead resolver.
func startSilentUpstream(t *testing.T) *countingUpstream {
	t.Helper()
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	require.NoError(t, err)
	u := &countingUpstream{addr: pc.LocalAddr().String()}
	go func() {
		buf := make([]byte, 4096)
		for {
			if _, _, err := pc.ReadFrom(buf); err != nil {
				return
			}
			u.queries.Add(1)
		}
	}()
	t.Cleanup(func() { _ = pc.Close() })
	return u
}

// startGuestPath runs northstar with the given upstreams behind a shim and
// returns the shim's address, the one a guest knows as 169.254.169.253.
func startGuestPath(t *testing.T, upstreams ...string) string {
	t.Helper()
	nsAddr := freeUDPAddr(t)
	srv, err := nsserver.NewServer(nsconfig.ServerConfig{
		Listen:  nsAddr,
		ZoneDir: t.TempDir(),
		Upstream: nsconfig.UpstreamConfig{
			Nameservers:        upstreams,
			AllowRecursionFrom: []string{"127.0.0.1/32"},
		},
	})
	require.NoError(t, err)
	require.NoError(t, srv.Start(t.Context()))
	t.Cleanup(func() { _ = srv.Shutdown(context.Background()) })

	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = pc.Close() })
	go newDNSForwarder([]string{nsAddr}).serveUDP(pc)
	return pc.LocalAddr().String()
}

func freeUDPAddr(t *testing.T) string {
	t.Helper()
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	require.NoError(t, err)
	addr := pc.LocalAddr().String()
	require.NoError(t, pc.Close())
	return addr
}

// guestResolve is one guest resolver attempt through the shim.
func guestResolve(t *testing.T, shim, name string, within time.Duration) {
	t.Helper()
	m := new(dns.Msg)
	m.SetQuestion(dns.Fqdn(name), dns.TypeA)
	m.RecursionDesired = true
	start := time.Now()
	r, _, err := (&dns.Client{Net: "udp", Timeout: guestAttempt}).Exchange(m, shim)
	took := time.Since(start).Round(time.Millisecond)
	require.NoErrorf(t, err, "guest lookup of %s through the shim failed after %s (a guest resolver gives up at %s)", name, took, guestAttempt)
	require.Equalf(t, dns.RcodeSuccess, r.Rcode, "guest lookup of %s: rcode %s after %s", name, dns.RcodeToString[r.Rcode], took)
	require.NotEmptyf(t, r.Answer, "guest lookup of %s: no answer", name)
	require.Lessf(t, time.Since(start), within, "guest lookup of %s took %s", name, took)
}

// Cell 26's `i/o timeout`: the first upstream is dead and the second is healthy.
func TestGuestPath_DeadFirstUpstream_GuestResolves(t *testing.T) {
	dead := startSilentUpstream(t)
	ok := startAnsweringUpstream(t)
	shim := startGuestPath(t, dead.addr, ok.addr)

	guestResolve(t, shim, "registry.example.", 2*time.Second)
	require.EqualValues(t, 1, ok.queries.Load(), "the healthy upstream did not answer")
}

// Cell 26's `server misbehaving`: the first upstream answers SERVFAIL.
func TestGuestPath_ServfailFirstUpstream_GuestResolves(t *testing.T) {
	bad := startRcodeUpstream(t, dns.RcodeServerFailure)
	ok := startAnsweringUpstream(t)
	shim := startGuestPath(t, bad.addr, ok.addr)

	guestResolve(t, shim, "registry.example.", 2*time.Second)
	require.EqualValues(t, 1, ok.queries.Load(), "the healthy upstream was not tried after SERVFAIL")
}
