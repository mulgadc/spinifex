//go:build lambdatestlink

package testlink

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os/user"
	"strconv"
	"sync"
	"time"

	"github.com/mulgadc/spinifex/spinifex/agents/lambda/hostlink"
	"golang.org/x/sys/unix"
)

// ControlName is the abstract Unix socket name the node's daemon serves. An
// abstract name has no directory entry; the kernel frees it when the last
// reference to the listening socket closes.
const ControlName = "spinifex-lambda-testlink-control/3"

// Service accounts at each end of the control socket.
const (
	GatewayUser = "spinifex-gw"
	DaemonUser  = "spinifex-daemon"
)

// joinAfterFunc arms f on ctx's cancellation and returns a stop that, unlike
// context.AfterFunc's, waits for f when it has already started.
func joinAfterFunc(ctx context.Context, f func()) func() {
	done := make(chan struct{})
	stop := context.AfterFunc(ctx, func() { defer close(done); f() })
	return func() {
		if !stop() {
			<-done
		}
	}
}

// MaxCredentialBytes bounds the encoded credential; a session's three values
// fit well inside it.
const MaxCredentialBytes = 8 << 10

// Declared control-socket bounds, independent of any message bus. Request and
// Reply payloads are capped raw; the frame cap leaves MetadataHeadroomBytes
// beside a full base64 payload, and metadata has no bound of its own.
const (
	MaxPayloadBytes       = 6 << 20
	MetadataHeadroomBytes = 64 << 10
	// payloadFieldSlack covers the payload key, quotes and comma.
	payloadFieldSlack = 64
	MaxFrameBytes     = int64((MaxPayloadBytes+2)/3*4 + MetadataHeadroomBytes + payloadFieldSlack)
	// MaxAdmitted bounds connections the daemon holds at once, from accept
	// to close; the daemon runs one attempt, so one more is answered busy.
	MaxAdmitted = 2
)

// Credential is one execution-role session for one attempt; it goes to the
// guest in the first frame and never on NATS, to disk or into a log.
type Credential = hostlink.Credential

// ErrNotSent means the exchange failed before any request byte was written, so
// the daemon cannot have received it.
var ErrNotSent = errors.New("test-link request not sent")

// ErrPeer means the far end of the control socket runs under an unexpected UID.
var ErrPeer = errors.New("test-link control peer has an unexpected UID")

// LookupUID returns the UID of a named service account.
func LookupUID(name string) (int, error) {
	u, err := user.Lookup(name)
	if err != nil {
		return 0, fmt.Errorf("look up %s: %w", name, err)
	}
	uid, err := strconv.Atoi(u.Uid)
	if err != nil {
		return 0, fmt.Errorf("uid of %s: %w", name, err)
	}
	return uid, nil
}

// ListenControl binds the abstract name. Any bind error, EADDRINUSE included
// while another socket holds the name, means this process must not serve.
// Go opens the listener close-on-exec, so no child inherits it.
func ListenControl(name string) (*net.UnixListener, error) {
	ln, err := net.ListenUnix("unix", &net.UnixAddr{Name: "@" + name, Net: "unix"})
	if err != nil {
		return nil, fmt.Errorf("bind test-link control socket: %w", err)
	}
	ln.SetUnlinkOnClose(false)
	return ln, nil
}

func peerUID(c *net.UnixConn) (int, error) {
	raw, err := c.SyscallConn()
	if err != nil {
		return 0, err
	}
	var cred *unix.Ucred
	var serr error
	if err := raw.Control(func(fd uintptr) {
		cred, serr = unix.GetsockoptUcred(int(fd), unix.SOL_SOCKET, unix.SO_PEERCRED)
	}); err != nil {
		return 0, err
	}
	if serr != nil {
		return 0, serr
	}
	return int(cred.Uid), nil
}

func writeFrame(w io.Writer, b []byte) error {
	var n [4]byte
	binary.BigEndian.PutUint32(n[:], uint32(len(b)))
	if _, err := w.Write(n[:]); err != nil {
		return err
	}
	_, err := w.Write(b)
	return err
}

func readFrame(r io.Reader, limit int64) ([]byte, error) {
	var n [4]byte
	if _, err := io.ReadFull(r, n[:]); err != nil {
		return nil, err
	}
	size := int64(binary.BigEndian.Uint32(n[:]))
	if size == 0 || size > limit {
		return nil, fmt.Errorf("frame of %d bytes outside 1..%d", size, limit)
	}
	b := make([]byte, size)
	if _, err := io.ReadFull(r, b); err != nil {
		return nil, err
	}
	return b, nil
}

// ControlServer answers one request per connection from the gateway's UID.
type ControlServer struct {
	GatewayUID int
	// RequestLimit bounds the encoded Request frame and ReplyLimit the
	// encoded Reply; a reply over it, or over MaxPayloadBytes, becomes unknown.
	RequestLimit int64
	ReplyLimit   int64
	// IOTimeout bounds reading the request and writing the reply.
	IOTimeout time.Duration
	Handle    func(ctx context.Context, req []byte, cred Credential) Reply
}

// Serve accepts until ctx ends or the listener fails, then waits for every
// connection it started.
// At most MaxAdmitted connections are held at once, each from accept to
// close; others wait unaccepted in the kernel's listen backlog.
func (s *ControlServer) Serve(ctx context.Context, ln *net.UnixListener) error {
	// Serve may return before the callback runs, so it closes the listener
	// too, and joins a callback already started before returning.
	defer joinAfterFunc(ctx, func() { _ = ln.Close() })()
	defer func() { _ = ln.Close() }()
	var wg sync.WaitGroup
	defer wg.Wait()
	slots := make(chan struct{}, MaxAdmitted)
	for {
		select {
		case slots <- struct{}{}:
		case <-ctx.Done():
			return nil
		}
		c, err := ln.AcceptUnix()
		if err != nil {
			<-slots
			if ctx.Err() != nil {
				return nil
			}
			return fmt.Errorf("accept test-link control: %w", err)
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() { <-slots }()
			// Closing on cancellation unblocks a read or write in progress; the
			// close is joined before the slot is released.
			defer joinAfterFunc(ctx, func() { _ = c.Close() })()
			s.serveConn(ctx, c)
		}()
	}
}

func (s *ControlServer) serveConn(ctx context.Context, c *net.UnixConn) {
	defer func() { _ = c.Close() }()
	// The peer is checked before any byte is read.
	if uid, err := peerUID(c); err != nil || uid != s.GatewayUID {
		slog.Warn("Lambda test link: control client refused", "uid", uid, "err", err)
		return
	}
	_ = c.SetReadDeadline(time.Now().Add(s.IOTimeout))
	req, err := readFrame(c, s.RequestLimit)
	if err != nil {
		slog.Warn("Lambda test link: control request unreadable", "err", err)
		return
	}
	rawCred, err := readFrame(c, MaxCredentialBytes)
	if err != nil {
		slog.Warn("Lambda test link: control credential unreadable", "err", err)
		return
	}
	var cred Credential
	if err := json.Unmarshal(rawCred, &cred); err != nil || !cred.Complete() {
		slog.Warn("Lambda test link: control credential malformed")
		return
	}
	_ = c.SetReadDeadline(time.Time{})

	rep := s.Handle(ctx, req, cred)
	// The raw payload is checked before the reply is encoded.
	if len(rep.Payload) > MaxPayloadBytes {
		rep = Reply{Outcome: OutcomeUnknown, Error: "result too large for the test path"}
	}
	body, err := json.Marshal(rep)
	if err == nil && int64(len(body)) > s.ReplyLimit {
		body, err = json.Marshal(Reply{Outcome: OutcomeUnknown, Error: "result too large for the test path"})
	}
	if err != nil {
		slog.Error("Lambda test link: encode reply", "err", err)
		return
	}
	_ = c.SetWriteDeadline(time.Now().Add(s.IOTimeout))
	if err := writeFrame(c, body); err != nil {
		slog.Error("Lambda test link: send reply", "err", err)
	}
}

// ControlClient sends one request to the daemon serving Name.
type ControlClient struct {
	Name         string
	DaemonUID    int
	RequestLimit int64
	ReplyLimit   int64
	// IOTimeout bounds writing the request; the reply has its own bound.
	IOTimeout time.Duration
	// writer, when set, wraps the connection the frames are written to; only
	// tests set it, to fail a write the daemon has already received.
	writer func(io.Writer) io.Writer
}

// Exchange writes one request and its credential and waits until replyBy, or
// ctx's end if earlier, for the one reply. An error wrapping ErrNotSent means
// nothing reached the daemon; any other error leaves the outcome unknown.
func (c *ControlClient) Exchange(ctx context.Context, req []byte, cred Credential, replyBy time.Time) (Reply, error) {
	if int64(len(req)) > c.RequestLimit {
		return Reply{}, fmt.Errorf("%w: encoded request of %d bytes exceeds the %d-byte limit", ErrNotSent, len(req), c.RequestLimit)
	}
	rawCred, err := json.Marshal(cred)
	if err != nil || !cred.Complete() || len(rawCred) > MaxCredentialBytes {
		return Reply{}, fmt.Errorf("%w: credential incomplete or over %d bytes", ErrNotSent, MaxCredentialBytes)
	}
	// A zero time would disable the deadline, and a past one leaves no time.
	if replyBy.IsZero() || !replyBy.After(time.Now()) {
		return Reply{}, fmt.Errorf("%w: reply bound is zero or already past", ErrNotSent)
	}
	// The dial stops at replyBy even when ctx has no deadline.
	dialCtx, cancelDial := context.WithDeadline(ctx, replyBy)
	var d net.Dialer
	conn, err := d.DialContext(dialCtx, "unix", "@"+c.Name)
	cancelDial()
	if err != nil {
		return Reply{}, fmt.Errorf("%w: dial test-link control: %w", ErrNotSent, err)
	}
	uc := conn.(*net.UnixConn)
	defer func() { _ = uc.Close() }()
	// The listener is checked before any byte is written.
	uid, err := peerUID(uc)
	if err != nil {
		return Reply{}, fmt.Errorf("%w: %w", ErrNotSent, err)
	}
	if uid != c.DaemonUID {
		return Reply{}, fmt.Errorf("%w: %w (uid %d)", ErrNotSent, ErrPeer, uid)
	}

	dl := newDeadlines(ctx, uc)
	defer dl.stop()
	// The client's write may not run past the reply bound. A frame already
	// buffered can still be processed later, so a timeout stays unknown.
	writeBy := time.Now().Add(c.IOTimeout)
	if replyBy.Before(writeBy) {
		writeBy = replyBy
	}
	if err := dl.set(earliest(ctx, writeBy)); err != nil {
		return Reply{}, fmt.Errorf("%w: %w", ErrNotSent, err)
	}
	if err := ctx.Err(); err != nil {
		return Reply{}, fmt.Errorf("%w: %w", ErrNotSent, err)
	}
	var w io.Writer = uc
	if c.writer != nil {
		w = c.writer(uc)
	}
	if err := writeFrame(w, req); err != nil {
		return Reply{}, fmt.Errorf("send test-link request: %w", err)
	}
	if err := writeFrame(w, rawCred); err != nil {
		return Reply{}, fmt.Errorf("send test-link request: %w", err)
	}
	if err := dl.set(earliest(ctx, replyBy)); err != nil {
		return Reply{}, fmt.Errorf("await test-link reply: %w", err)
	}
	body, err := readFrame(uc, c.ReplyLimit)
	if err != nil {
		return Reply{}, fmt.Errorf("read test-link reply: %w", err)
	}
	var rep Reply
	if err := json.Unmarshal(body, &rep); err != nil {
		return Reply{}, errors.New("unreadable test-link reply")
	}
	if len(rep.Payload) > MaxPayloadBytes {
		return Reply{}, fmt.Errorf("test-link reply payload of %d bytes exceeds %d", len(rep.Payload), MaxPayloadBytes)
	}
	return rep, nil
}

func earliest(ctx context.Context, t time.Time) time.Time {
	if d, ok := ctx.Deadline(); ok && d.Before(t) {
		return d
	}
	return t
}

// deadlines serialises deadline changes with ctx's cancellation, so a later
// set can never move a cancelled connection's deadline back into the future.
type deadlines struct {
	mu        sync.Mutex
	c         *net.UnixConn
	cancelled error
	stop      func() bool
}

func newDeadlines(ctx context.Context, c *net.UnixConn) *deadlines {
	d := &deadlines{c: c}
	d.stop = context.AfterFunc(ctx, func() {
		d.mu.Lock()
		defer d.mu.Unlock()
		d.cancelled = ctx.Err()
		if err := c.SetDeadline(time.Unix(1, 0)); err != nil {
			_ = c.Close()
		}
	})
	return d
}

func (d *deadlines) set(t time.Time) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.cancelled != nil {
		return d.cancelled
	}
	return d.c.SetDeadline(t)
}
