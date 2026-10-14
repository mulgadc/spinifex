package handlers_imds

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"
)

// hopLimitResolver serves a fakeResolver whose instance hop limit can change
// between requests, standing in for ModifyInstanceMetadataOptions.
type hopLimitResolver struct {
	fakeResolver

	hopLimit atomic.Int64
}

func (h *hopLimitResolver) resolveInstance(_ context.Context, _ *eniFacts) (*instanceFacts, error) {
	return &instanceFacts{putHopLimit: h.hopLimit.Load()}, nil
}

// putToken sends PUT /latest/api/token through h, recording the TTL the handler
// asked the connection for (0 if it asked for none).
func putToken(t *testing.T, res eniResolver) (*httptest.ResponseRecorder, int) {
	t.Helper()
	svc, _ := newTestService(res, &fakeIAM{}, &fakeAssumer{})
	var ttl int
	h := withTapConn(svc.httpHandler(), testENI(), func(v int) error { ttl = v; return nil })
	req := httptest.NewRequest(http.MethodPut, "http://"+MetaDataServerIP+pathToken, nil)
	req.Header.Set(hdrTokenTTL, "60")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec, ttl
}

func TestHTTP_TokenPUTSetsHopLimitTTL(t *testing.T) {
	cases := []struct {
		name string
		res  *fakeResolver
		want int
	}{
		{"default", &fakeResolver{eni: testENI(), inst: &instanceFacts{putHopLimit: 1}}, 1},
		{"raised", &fakeResolver{eni: testENI(), inst: &instanceFacts{putHopLimit: 2}}, 2},
		{"maximum", &fakeResolver{eni: testENI(), inst: &instanceFacts{putHopLimit: 64}}, 64},
		{"unset fails closed", &fakeResolver{eni: testENI(), inst: &instanceFacts{}}, 1},
		{"out of range fails closed", &fakeResolver{eni: testENI(), inst: &instanceFacts{putHopLimit: 65}}, 1},
		{"instance missing fails closed", &fakeResolver{eni: testENI()}, 1},
		{"lookup error fails closed", &fakeResolver{eni: testENI(), instErr: errors.New("gather timeout")}, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec, ttl := putToken(t, tc.res)
			require.Equal(t, http.StatusOK, rec.Code)
			assert.NotEmpty(t, rec.Body.String())
			assert.Equal(t, tc.want, ttl)
			assert.Equal(t, "close", rec.Header().Get("Connection"),
				"a keep-alive connection must not outlive the restricted TTL")
		})
	}
}

// ModifyInstanceMetadataOptions on a running instance applies to the next PUT.
func TestHTTP_TokenPUTFollowsModifiedHopLimit(t *testing.T) {
	res := &hopLimitResolver{fakeResolver: fakeResolver{eni: testENI()}}
	res.hopLimit.Store(1)
	_, ttl := putToken(t, res)
	assert.Equal(t, 1, ttl)

	res.hopLimit.Store(2)
	_, ttl = putToken(t, res)
	assert.Equal(t, 2, ttl)
}

// Without a TTL in place, the token must not be sent at all.
func TestHTTP_TokenPUTFailsClosedWithoutTTL(t *testing.T) {
	cases := []struct {
		name   string
		setTTL connTTLSetter
	}{
		{"no setter", nil},
		{"setter fails", func(int) error { return errors.New("setsockopt failed") }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			svc, _ := newTestService(&fakeResolver{eni: testENI()}, &fakeIAM{}, &fakeAssumer{})
			h := withTapConn(svc.httpHandler(), testENI(), tc.setTTL)
			req := httptest.NewRequest(http.MethodPut, "http://"+MetaDataServerIP+pathToken, nil)
			req.Header.Set(hdrTokenTTL, "60")
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			assert.Equal(t, http.StatusInternalServerError, rec.Code)
			assert.Empty(t, rec.Body.String())
		})
	}
}

// ttlProbeListener records each accepted socket's IP_TTL as the server closes
// it, so the TTL the response actually left with is observable after the fact.
type ttlProbeListener struct {
	net.Listener

	closed chan int
}

func (l *ttlProbeListener) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	tcp, ok := c.(*net.TCPConn)
	if !ok {
		_ = c.Close()
		return nil, errors.New("not a TCP conn")
	}
	return &ttlProbeConn{TCPConn: tcp, closed: l.closed}, nil
}

type ttlProbeConn struct {
	*net.TCPConn

	once   sync.Once
	closed chan int
}

func (c *ttlProbeConn) Close() error {
	c.once.Do(func() { c.closed <- socketTTL(c.TCPConn) })
	return c.TCPConn.Close()
}

func socketTTL(sc syscall.Conn) int {
	raw, err := sc.SyscallConn()
	if err != nil {
		return -1
	}
	ttl := -1
	_ = raw.Control(func(fd uintptr) {
		if v, err := unix.GetsockoptInt(int(fd), unix.IPPROTO_IP, unix.IP_TTL); err == nil {
			ttl = v
		}
	})
	return ttl
}

func awaitTTL(t *testing.T, closed <-chan int) int {
	t.Helper()
	select {
	case ttl := <-closed:
		return ttl
	case <-time.After(5 * time.Second):
		t.Fatal("server never closed the connection")
		return 0
	}
}

// The token connection's server-side IP_TTL follows HttpPutResponseHopLimit
// through the production responder; metadata GETs keep the kernel default.
func TestTapResponder_TokenResponseTTLIsHopLimit(t *testing.T) {
	const (
		eniID    = "eni-aaa"
		endpoint = "ime-aaa"
	)
	res := &hopLimitResolver{fakeResolver: fakeResolver{eni: testENI()}}
	res.hopLimit.Store(1)
	svc, _ := newTestService(res, &fakeIAM{}, &fakeAssumer{})

	closed := make(chan int, 4)
	var addr string
	listen := func(_ context.Context, _ string) (net.Listener, error) {
		ln, err := net.Listen("tcp4", "127.0.0.1:0")
		if err != nil {
			return nil, err
		}
		addr = ln.Addr().String()
		return &ttlProbeListener{Listener: ln, closed: closed}, nil
	}
	m := newTapResponderManager(svc.httpHandler(), staticResolve(map[string]*eniFacts{eniID: testENI()}), listen)
	require.NoError(t, m.start(context.Background(), eniID, endpoint))
	t.Cleanup(func() { m.stop(eniID) })

	token := putTapToken(t, addr)
	assert.Equal(t, 1, awaitTTL(t, closed), "default hop limit must send the token with TTL 1")

	res.hopLimit.Store(2)
	putTapToken(t, addr)
	assert.Equal(t, 2, awaitTTL(t, closed), "a modified hop limit must apply to the next PUT")

	ref, err := net.ListenPacket("udp4", "127.0.0.1:0")
	require.NoError(t, err)
	defaultTTL := socketTTL(ref.(*net.UDPConn))
	_ = ref.Close()

	client := &http.Client{Transport: &http.Transport{DisableKeepAlives: true}}
	req, err := http.NewRequest(http.MethodGet, "http://"+addr+prefixMetaData+"instance-id", nil)
	require.NoError(t, err)
	req.Header.Set(hdrToken, token)
	resp, err := client.Do(req)
	require.NoError(t, err)
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Equal(t, defaultTTL, awaitTTL(t, closed), "only the token response is hop-limited")
}

func TestLimitListener_CapsReleasesAndExposesSocket(t *testing.T) {
	inner, err := net.Listen("tcp4", "127.0.0.1:0")
	require.NoError(t, err)
	l := newLimitListener(inner, 1)
	defer func() { _ = l.Close() }()

	dial := func() net.Conn {
		c, err := net.Dial("tcp4", inner.Addr().String())
		require.NoError(t, err)
		t.Cleanup(func() { _ = c.Close() })
		return c
	}
	dial()
	dial()

	first, err := l.Accept()
	require.NoError(t, err)
	_, ok := first.(syscall.Conn)
	require.True(t, ok, "accepted conns must expose the socket for IP_TTL")
	require.NotNil(t, connTTLFromConn(first))
	require.NoError(t, connTTLFromConn(first)(7))
	assert.Equal(t, 7, socketTTL(first.(syscall.Conn)))

	accepted := make(chan net.Conn, 1)
	go func() {
		c, err := l.Accept()
		if err == nil {
			accepted <- c
		}
	}()
	select {
	case <-accepted:
		t.Fatal("Accept must block while the cap is reached")
	case <-time.After(100 * time.Millisecond):
	}

	require.NoError(t, first.Close())
	_ = first.Close() // a second Close must not release a second slot
	select {
	case c := <-accepted:
		_ = c.Close()
	case <-time.After(5 * time.Second):
		t.Fatal("closing a conn must release its slot")
	}
}

func TestLimitListener_CloseUnblocksAccept(t *testing.T) {
	inner, err := net.Listen("tcp4", "127.0.0.1:0")
	require.NoError(t, err)
	l := newLimitListener(inner, 1)
	l.sem <- struct{}{} // cap reached

	done := make(chan error, 1)
	go func() {
		_, err := l.Accept()
		done <- err
	}()
	require.NoError(t, l.Close())
	select {
	case err := <-done:
		assert.ErrorIs(t, err, net.ErrClosed)
	case <-time.After(5 * time.Second):
		t.Fatal("Close must unblock an Accept waiting for a slot")
	}
}
