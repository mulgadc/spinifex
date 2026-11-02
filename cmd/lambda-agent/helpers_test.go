package main

import (
	"context"
	"crypto/tls"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	lambdav1 "github.com/mulgadc/spinifex/contracts/lambda/v1"
	"github.com/mulgadc/spinifex/spinifex/runtime/compute/lambda/guestconn"
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
	conn chan accepted
}

func newHost(t *testing.T) *host {
	t.Helper()
	ln, err := net.ListenTCP("tcp", &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1)})
	require.NoError(t, err)
	t.Cleanup(func() { ln.Close() })
	gen, err := guestconn.NewGeneration(scope, time.Hour)
	require.NoError(t, err)
	return &host{ln: ln, gen: gen, conn: make(chan accepted, 1)}
}

func (h *host) entries() map[string][]byte { return h.gen.GuestEntries(h.ln.Addr().String()) }

type accepted struct {
	conn *tls.Conn
	err  error
}

// accept runs the host end until ctx ends and reports what it accepted.
func (h *host) accept(ctx context.Context) {
	go func() {
		c, err := h.gen.AcceptGuest(ctx, h.ln, 2*time.Second)
		h.conn <- accepted{c, err}
	}()
}

// guest waits for the host to authenticate the guest.
func (h *host) guest(t *testing.T) *tls.Conn {
	t.Helper()
	a := wait(t, h.conn, "host to authenticate the guest")
	require.NoError(t, a.err)
	return a.conn
}

// wait bounds every test wait so a stalled host or startup fails promptly.
func wait[T any](t *testing.T, ch <-chan T, what string) T {
	t.Helper()
	select {
	case v := <-ch:
		return v
	case <-time.After(10 * time.Second):
		t.Fatalf("timed out waiting for %s", what)
		panic("unreachable")
	}
}
