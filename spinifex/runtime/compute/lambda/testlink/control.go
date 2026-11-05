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

	"golang.org/x/sys/unix"
)

// ControlName is the abstract Unix socket name the node's daemon serves. An
// abstract name has no directory entry; the kernel frees it when the last
// reference to the listening socket closes.
const ControlName = "spinifex-lambda-testlink-control"

// Service accounts at each end of the control socket.
const (
	GatewayUser = "spinifex-gw"
	DaemonUser  = "spinifex-daemon"
)

// MaxCredentialBytes bounds the encoded credential; a session's three values
// fit well inside it.
const MaxCredentialBytes = 8 << 10

// Credential is one execution-role session for one attempt. It never goes on
// NATS, to disk or into a log.
type Credential struct {
	AccessKeyID     string `json:"access_key_id"`
	SecretAccessKey string `json:"secret_access_key"`
	SessionToken    string `json:"session_token"`
}

// LogValue keeps every value out of logs.
func (Credential) LogValue() slog.Value { return slog.StringValue("[redacted]") }

func (c Credential) complete() bool {
	return c.AccessKeyID != "" && c.SecretAccessKey != "" && c.SessionToken != ""
}

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
	// RequestLimit bounds the encoded Request alone, as the NATS path did, and
	// ReplyLimit the encoded Reply; a reply over it becomes unknown.
	RequestLimit int64
	ReplyLimit   int64
	// IOTimeout bounds reading the request and writing the reply.
	IOTimeout time.Duration
	Handle    func(ctx context.Context, req []byte, cred Credential) Reply
}

// Serve accepts until ctx ends or the listener fails, then waits for every
// connection it started.
func (s *ControlServer) Serve(ctx context.Context, ln *net.UnixListener) error {
	stop := context.AfterFunc(ctx, func() { _ = ln.Close() })
	defer stop()
	var wg sync.WaitGroup
	defer wg.Wait()
	for {
		c, err := ln.AcceptUnix()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return fmt.Errorf("accept test-link control: %w", err)
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
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
	if err := json.Unmarshal(rawCred, &cred); err != nil || !cred.complete() {
		slog.Warn("Lambda test link: control credential malformed")
		return
	}
	_ = c.SetReadDeadline(time.Time{})

	rep := s.Handle(ctx, req, cred)
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
	IOTimeout    time.Duration
}

// Exchange writes one request and its credential and reads the one reply. An
// error wrapping ErrNotSent means nothing reached the daemon; any other error
// means the request may have been delivered and its outcome is unknown.
func (c *ControlClient) Exchange(ctx context.Context, req []byte, cred Credential) (Reply, error) {
	if int64(len(req)) > c.RequestLimit {
		return Reply{}, fmt.Errorf("%w: encoded request of %d bytes exceeds the %d-byte limit", ErrNotSent, len(req), c.RequestLimit)
	}
	rawCred, err := json.Marshal(cred)
	if err != nil || !cred.complete() || len(rawCred) > MaxCredentialBytes {
		return Reply{}, fmt.Errorf("%w: credential incomplete or over %d bytes", ErrNotSent, MaxCredentialBytes)
	}
	var d net.Dialer
	conn, err := d.DialContext(ctx, "unix", "@"+c.Name)
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

	stop := context.AfterFunc(ctx, func() { _ = uc.SetDeadline(time.Now()) })
	defer stop()
	_ = uc.SetWriteDeadline(time.Now().Add(c.IOTimeout))
	if err := writeFrame(uc, req); err != nil {
		return Reply{}, fmt.Errorf("send test-link request: %w", err)
	}
	if err := writeFrame(uc, rawCred); err != nil {
		return Reply{}, fmt.Errorf("send test-link request: %w", err)
	}
	_ = uc.SetWriteDeadline(time.Time{})
	body, err := readFrame(uc, c.ReplyLimit)
	if err != nil {
		return Reply{}, fmt.Errorf("read test-link reply: %w", err)
	}
	var rep Reply
	if err := json.Unmarshal(body, &rep); err != nil {
		return Reply{}, errors.New("unreadable test-link reply")
	}
	return rep, nil
}
