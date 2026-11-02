package guestconn

import (
	"context"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	lambdav1 "github.com/mulgadc/spinifex/contracts/lambda/v1"
	"github.com/mulgadc/spinifex/spinifex/agents/lambda/hostlink"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	gen1 lambdav1.Scope = "ap-southeast-2/000000000001/fn-1/env-1/gen-1"
	gen2 lambdav1.Scope = "ap-southeast-2/000000000001/fn-1/env-1/gen-2"
)

func writeEntries(t *testing.T, entries map[string][]byte) string {
	t.Helper()
	dir := t.TempDir()
	for name, b := range entries {
		require.NoError(t, os.MkdirAll(filepath.Join(dir, name), 0o700))
		require.NoError(t, os.WriteFile(filepath.Join(dir, name, "raw"), b, 0o600))
	}
	return dir
}

func listen(t *testing.T) *net.TCPListener {
	t.Helper()
	ln, err := net.ListenTCP("tcp", &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1)})
	require.NoError(t, err)
	t.Cleanup(func() { ln.Close() })
	return ln
}

func newGeneration(t *testing.T, scope lambdav1.Scope) *Generation {
	t.Helper()
	g, err := NewGeneration(scope, time.Hour)
	require.NoError(t, err)
	return g
}

// guestFor loads the guest identity from g's fw_cfg entries, as the guest
// agent would.
func guestFor(t *testing.T, g *Generation, addr string) hostlink.Identity {
	t.Helper()
	id, err := hostlink.LoadIdentity(writeEntries(t, g.GuestEntries(addr)))
	require.NoError(t, err)
	return id
}

type accepted struct {
	conn net.Conn
	err  error
}

func acceptAsync(g *Generation, ln DeadlineListener, ctx context.Context, timeout time.Duration) <-chan accepted {
	out := make(chan accepted, 1)
	go func() {
		c, err := g.AcceptGuest(ctx, ln, timeout)
		out <- accepted{c, err}
	}()
	return out
}

func TestHostGuestExchange(t *testing.T) {
	ln := listen(t)
	g := newGeneration(t, gen1)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	hostSide := acceptAsync(g, ln, ctx, 5*time.Second)

	guest, err := hostlink.Dial(ctx, guestFor(t, g, ln.Addr().String()))
	require.NoError(t, err)
	defer guest.Close()
	host := <-hostSide
	require.NoError(t, host.err)
	defer host.conn.Close()

	_, err = host.conn.Write([]byte("host->guest"))
	require.NoError(t, err)
	buf := make([]byte, 11)
	_, err = io.ReadFull(guest, buf)
	require.NoError(t, err)
	assert.Equal(t, "host->guest", string(buf))

	_, err = guest.Write([]byte("guest->host"))
	require.NoError(t, err)
	_, err = io.ReadFull(host.conn, buf)
	require.NoError(t, err)
	assert.Equal(t, "guest->host", string(buf))
}

func TestGuestEntriesUseTheNamesTheGuestReads(t *testing.T) {
	g := newGeneration(t, gen1)
	id := guestFor(t, g, "10.0.0.1:1")
	assert.Equal(t, gen1, id.Scope)
	assert.Equal(t, gen1.URI(lambdav1.RoleHost), id.HostCert.URIs[0].String())
	assert.Equal(t, gen1.URI(lambdav1.RoleGuest), id.Cert.Leaf.URIs[0].String())
}

// A wrong guest is refused and the host keeps listening for the right one;
// so is a peer that connects and never handshakes.
func TestHostRefusesWrongGuestThenAcceptsTheRightOne(t *testing.T) {
	ln := listen(t)
	g := newGeneration(t, gen1)
	addr := ln.Addr().String()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	hostSide := acceptAsync(g, ln, ctx, 300*time.Millisecond)

	silent, err := net.Dial("tcp", addr)
	require.NoError(t, err)
	defer silent.Close()

	for name, wrong := range map[string]hostlink.Identity{
		"same scope, other generation's keys": guestFor(t, newGeneration(t, gen1), addr),
		"later generation":                    guestFor(t, newGeneration(t, gen2), addr),
	} {
		good := guestFor(t, g, addr)
		wrong.HostCert, wrong.Scope = good.HostCert, good.Scope // trust the real host, present the wrong guest
		c, err := hostlink.Dial(ctx, wrong)
		if err == nil {
			_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
			_, err = c.Read(make([]byte, 1))
			c.Close()
		}
		require.Error(t, err, name)
		assert.Contains(t, err.Error(), "bad certificate", name)
	}

	right, err := hostlink.Dial(ctx, guestFor(t, g, addr))
	require.NoError(t, err)
	defer right.Close()
	host := <-hostSide
	require.NoError(t, host.err)
	host.conn.Close()
}

// gatedListener holds the cancel callback's expiring SetDeadline until the
// test releases it, so a clear that does not wait would land first.
type gatedListener struct {
	entered, gate, cleared chan struct{}
	mu                     sync.Mutex
	deadline               time.Time
	calls                  int
}

func (l *gatedListener) Accept() (net.Conn, error) {
	<-l.entered
	return nil, errors.New("listener deadline expired")
}
func (l *gatedListener) Close() error   { return nil }
func (l *gatedListener) Addr() net.Addr { return &net.TCPAddr{} }
func (l *gatedListener) SetDeadline(d time.Time) error {
	if !d.IsZero() {
		close(l.entered)
		<-l.gate
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.calls++
	l.deadline = d
	if d.IsZero() && l.calls > 1 {
		close(l.cleared)
	}
	return nil
}

func TestAcceptGuestClearsDeadlineAfterCancelCallback(t *testing.T) {
	ln := &gatedListener{entered: make(chan struct{}), gate: make(chan struct{}), cleared: make(chan struct{})}
	ctx, cancel := context.WithCancel(context.Background())
	hostSide := acceptAsync(newGeneration(t, gen1), ln, ctx, time.Second)
	cancel()
	<-ln.entered

	// A clear that does not wait for the callback arrives now; one that does
	// cannot, so the bounded wait only delays the passing case.
	select {
	case <-ln.cleared:
	case <-time.After(200 * time.Millisecond):
	}
	close(ln.gate)
	require.ErrorIs(t, (<-hostSide).err, context.Canceled)

	ln.mu.Lock()
	defer ln.mu.Unlock()
	assert.True(t, ln.deadline.IsZero(), "listener left with expired deadline %v", ln.deadline)
	assert.Equal(t, 3, ln.calls)
}

func TestAcceptGuestStopsWithContext(t *testing.T) {
	ln := listen(t)
	g := newGeneration(t, gen1)
	ctx, cancel := context.WithCancelCause(context.Background())
	hostSide := acceptAsync(g, ln, ctx, time.Second)
	reason := errors.New("environment superseded")
	cancel(reason)

	select {
	case host := <-hostSide:
		assert.ErrorIs(t, host.err, reason)
	case <-time.After(5 * time.Second):
		t.Fatal("AcceptGuest did not return after cancellation")
	}

	// The listener is usable again by a later call.
	ctx2, cancel2 := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel2()
	hostSide = acceptAsync(g, ln, ctx2, time.Second)
	c, err := hostlink.Dial(ctx2, guestFor(t, g, ln.Addr().String()))
	require.NoError(t, err)
	defer c.Close()
	host := <-hostSide
	require.NoError(t, host.err)
	host.conn.Close()
}

func TestNewGenerationRejects(t *testing.T) {
	_, err := NewGeneration("a/b/c", time.Hour)
	assert.ErrorContains(t, err, "want 5 segments")
	_, err = NewGeneration(gen1, 0)
	assert.ErrorContains(t, err, "ttl must be positive")
	_, err = newGeneration(t, gen1).AcceptGuest(context.Background(), listen(t), 0)
	assert.ErrorContains(t, err, "handshake timeout must be positive")
}
