package guestmetadata

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/netip"
	"strconv"
	"sync"
	"syscall"
	"time"

	"github.com/mulgadc/predastore/endpointproxy"
	"github.com/mulgadc/spinifex/spinifex/domains/ec2/projection"
	"golang.org/x/sys/unix"
)

// endpointProxyConnsPerTap caps one guest's open endpoint sessions, as the
// metadata listener caps its connections, so a guest cannot exhaust vpcd's fds.
const endpointProxyConnsPerTap = 32

// endpointAdmitTimeout bounds the checks, the upstream dial and the header write,
// so a stalled read or a silent peer never holds an accept slot.
const endpointAdmitTimeout = 10 * time.Second

// EndpointView is one reconcile pass's view: the prefix-list entries a guest
// may dial and the gateway endpoint each subnet routes to.
type EndpointView struct {
	Entries   []netip.AddrPort
	Endpoints map[string]projection.GatewayEndpoint
}

// EndpointEgressFunc reports whether the account's security groups allow a new
// TCP connection to dst. An error refuses the connection.
type EndpointEgressFunc func(ctx context.Context, accountID string, groupIDs []string, dst netip.AddrPort) (bool, error)

// EndpointDialFunc opens the mutual-TLS session to the endpoint listener of the
// node at addr, handshake complete.
type EndpointDialFunc func(ctx context.Context, addr netip.Addr) (net.Conn, error)

// EndpointProxyConfig wires the per-tap endpoint proxy.
type EndpointProxyConfig struct {
	// Port is the proxy port the capture rules deliver to.
	Port   int
	Egress EndpointEgressFunc
	Dial   EndpointDialFunc
	// MaxAge bounds how long a published view admits connections without being
	// replaced, so a reconcile loop that stops publishing stops admitting.
	MaxAge time.Duration
	// Policies reports whether a new session may carry its endpoint's policy at
	// contract version 2. nil never does.
	Policies func() bool
}

// endpointView is a published view indexed for the per-connection checks.
type endpointView struct {
	entries   map[netip.AddrPort]struct{}
	endpoints map[string]projection.GatewayEndpoint
	at        time.Time
}

// effective reports whether the view still admits s: its destination is an
// entry, and its subnet routes to its endpoint under the policy it carries.
func (v *endpointView) effective(s *endpointSession) bool {
	if v == nil {
		return false
	}
	if _, ok := v.entries[s.dst]; !ok {
		return false
	}
	ep := v.endpoints[s.subnetID]
	return ep.ID == s.ep.ID && ep.Policy == s.ep.Policy
}

// endpointSession is one proxied connection, tracked so a pass can close it once
// its endpoint stops being effective for its subnet.
type endpointSession struct {
	eniID, subnetID string
	ep              projection.GatewayEndpoint
	src, dst        netip.AddrPort
	// contract is the version its header was written at.
	contract        uint8
	guest, upstream net.Conn
	once            sync.Once
}

// reset closes both halves, the guest's with an RST.
func (s *endpointSession) reset() {
	s.once.Do(func() {
		resetConn(s.guest)
		_ = s.upstream.Close()
	})
}

// EndpointProxy is the gateway endpoint proxy behind every tap: it admits a
// captured connection only against the latest view, the ENI's live record and
// its security groups, and relays it to the dialled node's endpoint listener.
type EndpointProxy struct {
	cfg       EndpointProxyConfig
	listen    tapListenFunc
	connAddrs func(net.Conn) (src, dst netip.AddrPort, err error)
	refresh   func(ctx context.Context, pinned *eniFacts) (*eniFacts, error)
	now       func() time.Time

	mu       sync.Mutex
	view     *endpointView // nil admits nothing
	sessions map[*endpointSession]struct{}
	// admitFailures limits warnings for admissions a failed read left undecided.
	admitFailures warnLimiter
	handlers      int // connections being admitted, dialled, relayed or closed
	// policyHandlers counts handlers that chose, or may yet choose, version 2.
	policyHandlers int
	closed         bool
	wg             sync.WaitGroup
}

// NewEndpointProxy validates cfg. Nothing is served until the IMDS service is
// given the proxy and a view is published.
func NewEndpointProxy(cfg EndpointProxyConfig) (*EndpointProxy, error) {
	switch {
	case cfg.Port <= 0 || cfg.Port > 65535:
		return nil, fmt.Errorf("endpoint proxy port %d is not a port", cfg.Port)
	case cfg.Egress == nil:
		return nil, errors.New("endpoint proxy: nil egress check")
	case cfg.Dial == nil:
		return nil, errors.New("endpoint proxy: nil dialer")
	case cfg.MaxAge <= 0:
		return nil, errors.New("endpoint proxy: view max age required")
	}
	return &EndpointProxy{
		cfg:       cfg,
		listen:    endpointProxyBinder(cfg.Port),
		connAddrs: socketAddrs,
		now:       time.Now,
		sessions:  map[*endpointSession]struct{}{},
	}, nil
}

// Publish replaces the view and closes every session it no longer admits.
func (p *EndpointProxy) Publish(v EndpointView) int {
	view := &endpointView{
		entries:   make(map[netip.AddrPort]struct{}, len(v.Entries)),
		endpoints: v.Endpoints,
		at:        p.now(),
	}
	for _, e := range v.Entries {
		view.entries[e] = struct{}{}
	}
	p.mu.Lock()
	p.view = view
	var stale []*endpointSession
	for s := range p.sessions {
		if !view.effective(s) || !p.contractHolds(s) {
			stale = append(stale, s)
			delete(p.sessions, s)
		}
	}
	p.mu.Unlock()
	for _, s := range stale {
		s.reset()
	}
	if len(stale) > 0 {
		slog.Info("IMDS: closed endpoint sessions no longer effective", "sessions", len(stale))
	}
	return len(stale)
}

// policiesActive reports whether a session may carry its policy.
func (p *EndpointProxy) policiesActive() bool {
	return p.cfg.Policies != nil && p.cfg.Policies()
}

// contractHolds reports whether s's version may still be served: a version 2
// session only while the cluster still enforces endpoint policies.
func (p *EndpointProxy) contractHolds(s *endpointSession) bool {
	return s.contract < endpointproxy.ContractVersion2 || p.policiesActive()
}

// WithdrawPolicies resets every version 2 session, once the cluster no longer
// enforces endpoint policies: an older gate would refuse them.
func (p *EndpointProxy) WithdrawPolicies() int {
	p.mu.Lock()
	var gone []*endpointSession
	for s := range p.sessions {
		if !p.contractHolds(s) {
			gone = append(gone, s)
			delete(p.sessions, s)
		}
	}
	p.mu.Unlock()
	for _, s := range gone {
		s.reset()
	}
	if len(gone) > 0 {
		slog.Info("IMDS: closed endpoint sessions carrying a policy the cluster no longer enforces", "sessions", len(gone))
	}
	return len(gone)
}

// PolicySessions is the number of handlers that chose version 2, or are about
// to choose, until both their sockets close.
func (p *EndpointProxy) PolicySessions() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.policyHandlers
}

// Invalidate withdraws the view, so no new connection is admitted until the
// next Publish. Open sessions continue, as established connections do on AWS.
func (p *EndpointProxy) Invalidate() {
	p.mu.Lock()
	p.view = nil
	p.mu.Unlock()
}

// Sessions is the number of guest connections not yet finished, counted from
// accept until both sockets are closed, so a rollback never sees a pending
// dial or a closing session as idle.
func (p *EndpointProxy) Sessions() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.handlers
}

// currentView returns the view if one is published and not past its age.
func (p *EndpointProxy) currentView() *endpointView {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.view == nil || p.now().Sub(p.view.at) > p.cfg.MaxAge {
		return nil
	}
	return p.view
}

// track counts a handler in, refusing once shutdown has begun, so Wait never
// races an Add.
func (p *EndpointProxy) track() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return false
	}
	p.handlers++
	p.wg.Add(1)
	return true
}

// untrack counts a handler out once its sockets are closed.
func (p *EndpointProxy) untrack() {
	p.mu.Lock()
	p.handlers--
	p.mu.Unlock()
	p.wg.Done()
}

// register records s, re-checking the view under the lock, so a Publish that
// withdrew the endpoint after the checks cannot miss the session.
func (p *EndpointProxy) register(s *endpointSession) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed || !p.view.effective(s) || !p.contractHolds(s) {
		return false
	}
	p.sessions[s] = struct{}{}
	return true
}

func (p *EndpointProxy) unregister(s *endpointSession) {
	p.mu.Lock()
	delete(p.sessions, s)
	p.mu.Unlock()
}

// closeTap resets the sessions of a tap whose responder stopped: the device
// their sockets are bound to is gone or replaced.
func (p *EndpointProxy) closeTap(eniID string) {
	p.mu.Lock()
	var gone []*endpointSession
	for s := range p.sessions {
		if s.eniID == eniID {
			gone = append(gone, s)
			delete(p.sessions, s)
		}
	}
	p.mu.Unlock()
	for _, s := range gone {
		s.reset()
	}
}

// shutdown refuses new handlers, resets every session and waits for the
// handlers to return. Listeners are closed by the responder manager first.
func (p *EndpointProxy) shutdown() {
	p.mu.Lock()
	p.closed = true
	p.view = nil
	all := make([]*endpointSession, 0, len(p.sessions))
	for s := range p.sessions {
		all = append(all, s)
	}
	clear(p.sessions)
	p.mu.Unlock()
	for _, s := range all {
		s.reset()
	}
	p.wg.Wait()
}

// serve accepts on one tap's listener until it is closed. It returns nil once
// closed, and the accept error otherwise, so the caller can rebind.
func (p *EndpointProxy) serve(ctx context.Context, ln net.Listener, pinned *eniFacts) error {
	for {
		c, err := ln.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return nil
			}
			return err
		}
		if !p.track() {
			resetConn(c)
			continue
		}
		go func() {
			defer p.untrack()
			p.handle(ctx, c, pinned)
		}()
	}
}

// handle admits, dials and relays one guest connection. Every failure resets
// the guest's connection; there is no other path to the gate.
func (p *EndpointProxy) handle(ctx context.Context, guest net.Conn, pinned *eniFacts) {
	admitCtx, cancel := context.WithTimeout(ctx, endpointAdmitTimeout)
	defer cancel()

	s, err := p.admit(admitCtx, guest, pinned)
	var header []byte
	if err == nil {
		var release func()
		header, release, err = p.header(s)
		defer release()
	}
	if err != nil {
		if errors.Is(err, errEndpointRefused) {
			slog.DebugContext(ctx, "IMDS: endpoint connection refused", "eni_id", pinned.eniID, "err", err)
		} else if n, ok := p.admitFailures.allow(p.now()); ok {
			slog.WarnContext(ctx, "IMDS: endpoint connection could not be checked, connection reset",
				"eni_id", pinned.eniID, "err", err, "suppressed", n)
		}
		resetConn(guest)
		return
	}
	up, err := p.cfg.Dial(admitCtx, s.dst.Addr())
	if err != nil {
		slog.WarnContext(ctx, "IMDS: endpoint channel unavailable, connection reset",
			"eni_id", pinned.eniID, "dst", s.dst.String(), "err", err)
		resetConn(guest)
		return
	}
	s.upstream = up
	if deadline, ok := admitCtx.Deadline(); ok {
		_ = up.SetWriteDeadline(deadline)
	}
	if _, err := up.Write(header); err != nil {
		slog.WarnContext(ctx, "IMDS: endpoint header write failed, connection reset",
			"eni_id", pinned.eniID, "dst", s.dst.String(), "err", err)
		s.reset()
		return
	}
	_ = up.SetWriteDeadline(time.Time{})
	if !p.register(s) {
		s.reset()
		return
	}
	defer p.unregister(s)
	splice(s)
}

// errEndpointRefused marks an admission refused by policy, as opposed to one
// that could not be decided because a read failed.
var errEndpointRefused = errors.New("refused")

// endpointWarnInterval spaces the warnings for admissions that could not be
// decided, so an outage logs once per interval rather than per connection.
const endpointWarnInterval = 10 * time.Second

// warnLimiter lets one warning through per endpointWarnInterval and counts the
// ones it held back.
type warnLimiter struct {
	mu         sync.Mutex
	last       time.Time
	suppressed int
}

func (w *warnLimiter) allow(now time.Time) (int, bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if !w.last.IsZero() && now.Sub(w.last) < endpointWarnInterval {
		w.suppressed++
		return 0, false
	}
	n := w.suppressed
	w.last, w.suppressed = now, 0
	return n, true
}

// header picks s's contract version and encodes its header. A handler is
// counted toward version 2 before the level is read, so a rollback that reads
// none counted cannot be overtaken by one that read the level and paused. The
// caller holds release until both sockets close.
func (p *EndpointProxy) header(s *endpointSession) ([]byte, func(), error) {
	release := func() {}
	s.contract = endpointproxy.ContractVersion1
	if p.cfg.Policies != nil {
		p.mu.Lock()
		p.policyHandlers++
		p.mu.Unlock()
		var once sync.Once
		release = func() {
			once.Do(func() {
				p.mu.Lock()
				p.policyHandlers--
				p.mu.Unlock()
			})
		}
		if p.cfg.Policies() {
			s.contract = endpointproxy.ContractVersion2
		} else {
			release()
		}
	}
	h := endpointproxy.Header{
		Version: s.contract, Source: s.src, Destination: s.dst,
		VpceID: s.ep.ID, VpcID: s.ep.VpcID, AccountID: s.ep.AccountID,
	}
	switch {
	case s.contract == endpointproxy.ContractVersion2:
		if s.ep.Generation <= 0 {
			return nil, release, fmt.Errorf("endpoint %s has no policy generation", s.ep.ID)
		}
		h.PolicyGeneration, h.Policy = uint64(s.ep.Generation), s.ep.PolicyDocument()
	case s.ep.Policy != "":
		// Version 1 carries no policy, so the gate would allow what it denies.
		return nil, release, fmt.Errorf("%w: endpoint %s holds a custom policy the cluster does not enforce", errEndpointRefused, s.ep.ID)
	}
	b, err := h.Marshal()
	return b, release, err
}

// admit runs the checks a captured connection must pass before it is dialled:
// the destination is a current entry, the ENI's subnet routes to an endpoint,
// and the ENI's security groups allow the egress.
func (p *EndpointProxy) admit(ctx context.Context, guest net.Conn, pinned *eniFacts) (*endpointSession, error) {
	src, dst, err := p.connAddrs(guest)
	if err != nil {
		return nil, err
	}
	view := p.currentView()
	if view == nil {
		return nil, errors.New("no current endpoint view")
	}
	if _, ok := view.entries[dst]; !ok {
		return nil, fmt.Errorf("%w: destination %s is not a prefix-list entry", errEndpointRefused, dst)
	}
	eni, err := p.refresh(ctx, pinned)
	if err != nil {
		return nil, fmt.Errorf("read eni: %w", err)
	}
	if eni == nil {
		return nil, fmt.Errorf("%w: eni record gone", errEndpointRefused)
	}
	// The demux matches on the tap, not the source, so a forged source would
	// otherwise reach the gate as aws:VpcSourceIp.
	if ip, perr := netip.ParseAddr(eni.privateIP); perr != nil || ip != src.Addr() {
		return nil, fmt.Errorf("%w: source %s is not the interface's address", errEndpointRefused, src.Addr())
	}
	ep, ok := view.endpoints[eni.subnetID]
	if !ok || ep.VpcID != eni.vpcID {
		return nil, fmt.Errorf("%w: subnet %s routes to no gateway endpoint", errEndpointRefused, eni.subnetID)
	}
	allowed, err := p.cfg.Egress(ctx, eni.accountID, eni.securityGroupIDs, dst)
	if err != nil {
		return nil, fmt.Errorf("evaluate security groups: %w", err)
	}
	if !allowed {
		return nil, fmt.Errorf("%w: security groups deny egress to %s", errEndpointRefused, dst)
	}
	return &endpointSession{eniID: pinned.eniID, subnetID: eni.subnetID, ep: ep, src: src, dst: dst, guest: guest}, nil
}

// splice copies both ways, passing a clean end of either stream on as a
// half-close. A failure in either direction tears both down.
func splice(s *endpointSession) {
	var wg sync.WaitGroup
	cp := func(dst, src net.Conn) {
		defer wg.Done()
		if _, err := io.Copy(dst, src); err != nil {
			s.reset()
			return
		}
		closeWrite(dst)
	}
	wg.Add(2)
	go cp(s.upstream, s.guest)
	go cp(s.guest, s.upstream)
	wg.Wait()
	_ = s.guest.Close()
	_ = s.upstream.Close()
}

// socketAddrs reads the guest's address and, because the listener is
// transparent, the destination it dialled from the accepted socket.
func socketAddrs(c net.Conn) (src, dst netip.AddrPort, err error) {
	r, rok := c.RemoteAddr().(*net.TCPAddr)
	l, lok := c.LocalAddr().(*net.TCPAddr)
	if !rok || !lok {
		return src, dst, errors.New("connection is not TCP")
	}
	src = netip.AddrPortFrom(r.AddrPort().Addr().Unmap(), r.AddrPort().Port())
	dst = netip.AddrPortFrom(l.AddrPort().Addr().Unmap(), l.AddrPort().Port())
	return src, dst, nil
}

// unwrapConn returns the socket under a capped connection.
func unwrapConn(c net.Conn) net.Conn {
	if lc, ok := c.(*limitConn); ok {
		return lc.Conn
	}
	return c
}

// resetConn closes c with an RST, which is how a refused or withdrawn endpoint
// connection ends for the guest.
func resetConn(c net.Conn) {
	if l, ok := unwrapConn(c).(interface{ SetLinger(int) error }); ok {
		_ = l.SetLinger(0)
	}
	_ = c.Close()
}

func closeWrite(c net.Conn) {
	if w, ok := unwrapConn(c).(interface{ CloseWrite() error }); ok {
		_ = w.CloseWrite()
	}
}

// endpointProxyBinder binds 0.0.0.0:port on the tap's endpoint. IP_TRANSPARENT
// lets it accept the TPROXY-delivered connections, whose local address is then
// the entry the guest dialled.
func endpointProxyBinder(port int) tapListenFunc {
	return func(ctx context.Context, endpoint string) (net.Listener, error) {
		bind := bindToDeviceControl(endpoint)
		lc := net.ListenConfig{Control: func(network, address string, c syscall.RawConn) error {
			if err := bind(network, address, c); err != nil {
				return err
			}
			var sockErr error
			if err := c.Control(func(fd uintptr) {
				sockErr = unix.SetsockoptInt(int(fd), unix.SOL_IP, unix.IP_TRANSPARENT, 1)
			}); err != nil {
				return err
			}
			return sockErr
		}}
		return lc.Listen(ctx, "tcp4", net.JoinHostPort("0.0.0.0", strconv.Itoa(port)))
	}
}
