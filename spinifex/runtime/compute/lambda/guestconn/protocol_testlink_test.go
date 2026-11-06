//go:build lambdatestlink

//test:in-package — reuses the package's in-package handshake fixtures newGeneration, listen and guestFor
package guestconn

import (
	"context"
	"crypto/tls"
	"net"
	"testing"
	"time"

	lambdav1 "github.com/mulgadc/spinifex/contracts/lambda/v1"
	"github.com/mulgadc/spinifex/spinifex/agents/lambda/hostlink"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// releaseOnly is cfg as a release build would offer it: ALPNv1 alone.
func releaseOnly(cfg *tls.Config) *tls.Config {
	return protocolOnly(cfg, lambdav1.ALPNv1)
}

func protocolOnly(cfg *tls.Config, protocol string) *tls.Config {
	cfg = cfg.Clone()
	cfg.NextProtos = []string{protocol}
	verify := cfg.VerifyConnection
	cfg.VerifyConnection = func(cs tls.ConnectionState) error {
		cs.NegotiatedProtocol = hostlink.Protocol // let only the ALPN offer differ
		return verify(cs)
	}
	return cfg
}

func TestTestLinkRejectsPreviousProtocolBothWays(t *testing.T) {
	const previous = "spinifex-lambda-testlink/2"
	t.Run("old guest", func(t *testing.T) {
		ln := listen(t)
		g := newGeneration(t, gen1)
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		hostSide := acceptAsync(g, ln, ctx, 2*time.Second)
		id := guestFor(t, g, ln.Addr().String())
		raw, err := net.Dial("tcp", ln.Addr().String())
		require.NoError(t, err)
		defer raw.Close()
		require.Error(t, tls.Client(raw, protocolOnly(id.ClientConfig(), previous)).HandshakeContext(ctx))
		require.Error(t, (<-hostSide).err)
	})
	t.Run("old host", func(t *testing.T) {
		ln := listen(t)
		g := newGeneration(t, gen1)
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		hostErr := make(chan error, 1)
		go func() {
			c, err := ln.Accept()
			if err != nil {
				hostErr <- err
				return
			}
			defer c.Close()
			hostErr <- tls.Server(c, protocolOnly(g.ServerConfig(), previous)).HandshakeContext(ctx)
		}()
		_, err := hostlink.Dial(ctx, guestFor(t, g, ln.Addr().String()))
		require.Error(t, err)
		require.Error(t, <-hostErr)
	})
}

func TestTestLinkProtocolIsNotTheReleaseProtocol(t *testing.T) {
	assert.NotEqual(t, lambdav1.ALPNv1, hostlink.Protocol)
}

// A test-link host and a guest offering only the release protocol share none,
// so the handshake fails before any frame.
func TestTestLinkHostRefusesAReleaseGuest(t *testing.T) {
	ln := listen(t)
	g := newGeneration(t, gen1)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	hostSide := acceptAsync(g, ln, ctx, 2*time.Second)

	id := guestFor(t, g, ln.Addr().String())
	raw, err := net.Dial("tcp", ln.Addr().String())
	require.NoError(t, err)
	defer raw.Close()
	guest := tls.Client(raw, releaseOnly(id.ClientConfig()))
	require.Error(t, guest.HandshakeContext(ctx), "the guest gets no session")

	host := <-hostSide
	require.Error(t, host.err, "the host accepts no guest")
}

func TestTestLinkGuestRefusesAReleaseHost(t *testing.T) {
	ln := listen(t)
	g := newGeneration(t, gen1)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	hostErr := make(chan error, 1)
	go func() {
		c, err := ln.Accept()
		if err != nil {
			hostErr <- err
			return
		}
		defer c.Close()
		hostErr <- tls.Server(c, releaseOnly(g.ServerConfig())).HandshakeContext(ctx)
	}()

	_, err := hostlink.Dial(ctx, guestFor(t, g, ln.Addr().String()))
	require.Error(t, err, "the guest never reaches the first frame")
	require.Error(t, <-hostErr)
}
