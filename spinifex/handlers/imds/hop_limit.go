package handlers_imds

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"sync"
	"syscall"

	"golang.org/x/sys/unix"
)

// defaultPutHopLimit is the HttpPutResponseHopLimit the token response falls
// back to whenever the instance's own value cannot be established.
const defaultPutHopLimit = 1

// maxPutHopLimit is the AWS-valid upper bound for HttpPutResponseHopLimit.
const maxPutHopLimit = 64

// connTTLSetter sets the IP TTL of the connection a request arrived on.
type connTTLSetter func(ttl int) error

// connTTLFromConn returns a setter for c's socket, or nil if c exposes no socket.
func connTTLFromConn(c net.Conn) connTTLSetter {
	sc, ok := c.(syscall.Conn)
	if !ok {
		return nil
	}
	return func(ttl int) error {
		raw, err := sc.SyscallConn()
		if err != nil {
			return err
		}
		var sockErr error
		if err := raw.Control(func(fd uintptr) {
			sockErr = unix.SetsockoptInt(int(fd), unix.IPPROTO_IP, unix.IP_TTL, ttl)
		}); err != nil {
			return err
		}
		return sockErr
	}
}

// withConnTTL is the http.Server.ConnContext that lets handleToken reach the
// accepted socket. A conn without one gets no setter, so the PUT fails closed.
func withConnTTL(ctx context.Context, c net.Conn) context.Context {
	if set := connTTLFromConn(c); set != nil {
		return context.WithValue(ctx, ctxKeyConnTTL, set)
	}
	return ctx
}

var errNoConnTTL = errors.New("no TTL setter for request connection")

// setRequestConnTTL sets the IP TTL of the connection r's context came from.
func setRequestConnTTL(ctx context.Context, ttl int) error {
	set, _ := ctx.Value(ctxKeyConnTTL).(connTTLSetter)
	if set == nil {
		return errNoConnTTL
	}
	return set(ttl)
}

// putResponseHopLimit resolves the instance's current HttpPutResponseHopLimit
// on every call, so ModifyInstanceMetadataOptions applies on the next PUT. A
// lookup failure, missing instance, or unset or out-of-range value yields 1.
func (s *IMDSServiceImpl) putResponseHopLimit(ctx context.Context, eni *eniFacts) int {
	inst, err := s.resolver.resolveInstance(ctx, eni)
	if err != nil {
		slog.WarnContext(ctx, "IMDS: hop limit lookup failed, using default",
			"instance_id", eni.instanceID, "hop_limit", defaultPutHopLimit, "err", err)
		return defaultPutHopLimit
	}
	if inst == nil || inst.putHopLimit < 1 || inst.putHopLimit > maxPutHopLimit {
		return defaultPutHopLimit
	}
	return int(inst.putHopLimit)
}

// limitListener caps concurrently open conns like netutil.LimitListener, but
// its conns still expose the accepted socket so the token PUT can set IP_TTL.
type limitListener struct {
	net.Listener

	sem       chan struct{}
	closeOnce sync.Once
	done      chan struct{}
}

func newLimitListener(l net.Listener, n int) *limitListener {
	return &limitListener{Listener: l, sem: make(chan struct{}, n), done: make(chan struct{})}
}

// Accept blocks while n conns are open. It returns net.ErrClosed once the
// listener is closed, even while waiting for a slot.
func (l *limitListener) Accept() (net.Conn, error) {
	select {
	case l.sem <- struct{}{}:
	case <-l.done:
		return nil, net.ErrClosed
	}
	c, err := l.Listener.Accept()
	if err != nil {
		<-l.sem
		return nil, err
	}
	return &limitConn{Conn: c, release: func() { <-l.sem }}, nil
}

func (l *limitListener) Close() error {
	err := l.Listener.Close()
	l.closeOnce.Do(func() { close(l.done) })
	return err
}

// limitConn releases its listener slot on the first Close.
type limitConn struct {
	net.Conn

	releaseOnce sync.Once
	release     func()
}

var _ syscall.Conn = (*limitConn)(nil)

func (c *limitConn) Close() error {
	err := c.Conn.Close()
	c.releaseOnce.Do(c.release)
	return err
}

// SyscallConn exposes the wrapped conn's socket.
func (c *limitConn) SyscallConn() (syscall.RawConn, error) {
	sc, ok := c.Conn.(syscall.Conn)
	if !ok {
		return nil, errors.New("wrapped conn exposes no socket")
	}
	return sc.SyscallConn()
}
