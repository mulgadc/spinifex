//go:build !lambdatestlink

package main

import (
	"context"
	"crypto/tls"
	"errors"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	lambdav1 "github.com/mulgadc/spinifex/contracts/lambda/v1"
	"github.com/mulgadc/spinifex/spinifex/runtime/compute/lambda/guestconn"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const scope lambdav1.Scope = "ap-southeast-2/000000000001/fn-1/env-1/gen-1"

// guest is a fake fw_cfg tree; unload removes it as unloading the module would.
type guest struct {
	root     string
	unloads  int
	sealedAt chan struct{}
}

func newGuest(t *testing.T, entries map[string][]byte) *guest {
	t.Helper()
	g := &guest{root: filepath.Join(t.TempDir(), "qemu_fw_cfg"), sealedAt: make(chan struct{}, 1)}
	for name, b := range entries {
		dir := filepath.Join(g.root, "by_name", lambdav1.FwCfgPrefix, name)
		require.NoError(t, os.MkdirAll(dir, 0o700))
		require.NoError(t, os.WriteFile(filepath.Join(dir, "raw"), b, 0o600))
	}
	return g
}

func (g *guest) startup() startup {
	return startup{
		fwCfgDir:  filepath.Join(g.root, "by_name", lambdav1.FwCfgPrefix),
		fwCfgRoot: g.root,
		unload: func() error {
			g.unloads++
			g.sealedAt <- struct{}{}
			return os.RemoveAll(g.root)
		},
	}
}

type host struct {
	ln   *net.TCPListener
	gen  *guestconn.Generation
	conn chan *tls.Conn
}

func newHost(t *testing.T) *host {
	t.Helper()
	ln, err := net.ListenTCP("tcp", &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1)})
	require.NoError(t, err)
	t.Cleanup(func() { ln.Close() })
	gen, err := guestconn.NewGeneration(scope, time.Hour)
	require.NoError(t, err)
	return &host{ln: ln, gen: gen, conn: make(chan *tls.Conn, 1)}
}

func (h *host) entries() map[string][]byte { return h.gen.GuestEntries(h.ln.Addr().String()) }

// accept runs the host end until ctx ends, handing over an authenticated guest.
func (h *host) accept(ctx context.Context) {
	go func() {
		c, err := h.gen.AcceptGuest(ctx, h.ln, 2*time.Second)
		if err == nil {
			h.conn <- c
		}
	}()
}

type result struct {
	code int
	err  error
}

func runAsync(ctx context.Context, s startup) <-chan result {
	out := make(chan result, 1)
	go func() {
		code, err := s.run(ctx)
		out <- result{code, err}
	}()
	return out
}

func TestStartupAuthenticatesAfterSealingFwCfg(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	h := newHost(t)
	h.accept(ctx)
	g := newGuest(t, h.entries())
	done := runAsync(ctx, g.startup())

	conn := <-h.conn
	select {
	case <-g.sealedAt:
	default:
		t.Fatal("guest dialled before fw_cfg was sealed")
	}
	_, err := os.Stat(g.root)
	assert.ErrorIs(t, err, os.ErrNotExist, "fw_cfg still readable while connected")

	require.NoError(t, conn.Close())
	res := <-done
	assert.Equal(t, exitClosed, res.code)
	assert.NoError(t, res.err)
	assert.Equal(t, 1, g.unloads)
}

func TestStartupRefusesHostData(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	h := newHost(t)
	h.accept(ctx)
	done := runAsync(ctx, newGuest(t, h.entries()).startup())

	conn := <-h.conn
	defer conn.Close()
	_, err := conn.Write([]byte{0})
	require.NoError(t, err)
	res := <-done
	assert.Equal(t, exitLink, res.code)
	assert.ErrorIs(t, res.err, errNoProtocol)
}

// The host pins another generation's guest; the guest learns of the refusal
// on its first read and fails with a link error, never a clean close.
func TestStartupFailsWhenHostRefusesGuest(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	h := newHost(t)
	h.accept(ctx)
	other, err := guestconn.NewGeneration(scope, time.Hour)
	require.NoError(t, err)
	entries := h.entries()
	wrong := other.GuestEntries(h.ln.Addr().String())
	entries[lambdav1.EntryGuestKey], entries[lambdav1.EntryGuestCert] = wrong[lambdav1.EntryGuestKey], wrong[lambdav1.EntryGuestCert]
	g := newGuest(t, entries)

	res := <-runAsync(ctx, g.startup())
	assert.Equal(t, exitLink, res.code)
	require.Error(t, res.err)
	assert.Contains(t, res.err.Error(), "bad certificate")
	assert.Equal(t, 1, g.unloads)
	select {
	case <-h.conn:
		t.Fatal("host accepted a guest it does not pin")
	default:
	}
}

func TestStartupRefusesWithoutDialling(t *testing.T) {
	h := newHost(t)
	cases := map[string]struct {
		entries map[string][]byte
		unload  func(g *guest) func() error
		want    string
	}{
		"missing entry": {
			entries: func() map[string][]byte { e := h.entries(); delete(e, lambdav1.EntryHostCert); return e }(),
			want:    "read host-cert",
		},
		"fw_cfg stays readable": {
			entries: h.entries(),
			unload:  func(*guest) func() error { return func() error { return errors.New("module is built in") } },
			want:    "fw_cfg still readable",
		},
		"unload claims success but tree remains": {
			entries: h.entries(),
			unload:  func(*guest) func() error { return func() error { return nil } },
			want:    "unload reported success",
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			g := newGuest(t, tc.entries)
			s := g.startup()
			if tc.unload != nil {
				s.unload = tc.unload(g)
			}
			code, err := s.run(context.Background())
			assert.Equal(t, exitRefused, code)
			assert.ErrorContains(t, err, tc.want)
		})
	}

	// Nothing dialled the host in any case above.
	require.NoError(t, h.ln.SetDeadline(time.Now().Add(50*time.Millisecond)))
	_, err := h.ln.Accept()
	var ne net.Error
	require.ErrorAs(t, err, &ne)
	assert.True(t, ne.Timeout())
}

// A missing entry still seals fw_cfg: the key may be present even when
// another entry is not.
func TestStartupSealsEvenWhenIdentityIsIncomplete(t *testing.T) {
	h := newHost(t)
	e := h.entries()
	delete(e, lambdav1.EntryHostAddr)
	g := newGuest(t, e)
	code, err := g.startup().run(context.Background())
	assert.Equal(t, exitRefused, code)
	assert.ErrorContains(t, err, "read host-addr")
	assert.Equal(t, 1, g.unloads)
	_, statErr := os.Stat(g.root)
	assert.ErrorIs(t, statErr, os.ErrNotExist)
}

func TestStartupStopsWithContext(t *testing.T) {
	ctx, cancel := context.WithCancelCause(context.Background())
	defer cancel(nil)
	h := newHost(t)
	h.accept(ctx)
	done := runAsync(ctx, newGuest(t, h.entries()).startup())
	conn := <-h.conn
	defer conn.Close()

	reason := errors.New("guest shutting down")
	cancel(reason)
	res := <-done
	assert.Equal(t, exitLink, res.code)
	assert.ErrorIs(t, res.err, reason)
}
