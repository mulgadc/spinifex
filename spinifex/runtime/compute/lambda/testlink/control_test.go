//go:build lambdatestlink

package testlink

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"io"
	"net"
	"os"
	"os/exec"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"
)

const testRequestLimit = 1024

var testCred = Credential{AccessKeyID: "ASIATESTKEY", SecretAccessKey: "secret-value", SessionToken: "token-value"}

func controlName(t *testing.T) string {
	t.Helper()
	b := make([]byte, 8)
	_, err := rand.Read(b)
	require.NoError(t, err)
	return "spinifex-test-" + hex.EncodeToString(b)
}

type served struct {
	calls atomic.Int32
	req   atomic.Value
	cred  atomic.Value
}

func serveControl(t *testing.T, name string, gatewayUID int, handle func(context.Context, []byte, Credential) Reply) (*served, context.CancelFunc) {
	t.Helper()
	ln, err := ListenControl(name)
	require.NoError(t, err)
	s := &served{}
	srv := &ControlServer{GatewayUID: gatewayUID, RequestLimit: testRequestLimit, ReplyLimit: 4096, IOTimeout: 5 * time.Second,
		Handle: func(ctx context.Context, req []byte, cred Credential) Reply {
			s.calls.Add(1)
			s.req.Store(bytes.Clone(req))
			s.cred.Store(cred)
			if handle != nil {
				return handle(ctx, req, cred)
			}
			return Reply{Outcome: OutcomeDone, Payload: []byte("ok")}
		}}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _ = srv.Serve(ctx, ln); close(done) }()
	stop := func() { cancel(); <-done }
	t.Cleanup(stop)
	return s, stop
}

func client(name string, daemonUID int) *ControlClient {
	return &ControlClient{Name: name, DaemonUID: daemonUID, RequestLimit: testRequestLimit, ReplyLimit: 4096, IOTimeout: 5 * time.Second}
}

func soon() time.Time { return time.Now().Add(5 * time.Second) }

func TestControl_OneRequestOneReply(t *testing.T) {
	name := controlName(t)
	s, _ := serveControl(t, name, os.Getuid(), nil)

	rep, err := client(name, os.Getuid()).Exchange(t.Context(), []byte(`{"attempt_id":"a"}`), testCred, soon())
	require.NoError(t, err)
	assert.Equal(t, OutcomeDone, rep.Outcome)
	assert.Equal(t, []byte("ok"), rep.Payload)
	assert.Equal(t, int32(1), s.calls.Load())
	assert.Equal(t, []byte(`{"attempt_id":"a"}`), s.req.Load())
	assert.Equal(t, testCred, s.cred.Load())
}

func TestControl_RequestSizeBoundary(t *testing.T) {
	name := controlName(t)
	s, _ := serveControl(t, name, os.Getuid(), nil)
	c := client(name, os.Getuid())

	_, err := c.Exchange(t.Context(), bytes.Repeat([]byte("x"), testRequestLimit), testCred, soon())
	require.NoError(t, err, "a request exactly at the limit is delivered with its credential")
	assert.Equal(t, testCred, s.cred.Load())

	_, err = c.Exchange(t.Context(), bytes.Repeat([]byte("x"), testRequestLimit+1), testCred, soon())
	require.ErrorIs(t, err, ErrNotSent)
	big := testCred
	big.SessionToken = string(bytes.Repeat([]byte("t"), MaxCredentialBytes))
	_, err = c.Exchange(t.Context(), []byte("{}"), big, soon())
	require.ErrorIs(t, err, ErrNotSent)
	_, err = c.Exchange(t.Context(), []byte("{}"), Credential{AccessKeyID: "ASIA"}, soon())
	require.ErrorIs(t, err, ErrNotSent)
	assert.Equal(t, int32(1), s.calls.Load(), "nothing over a bound reaches the daemon")
}

func TestControl_NameIsASingleton(t *testing.T) {
	name := controlName(t)
	serveControl(t, name, os.Getuid(), nil)

	_, err := ListenControl(name)
	require.ErrorIs(t, err, unix.EADDRINUSE)
	_, err = client(name, os.Getuid()).Exchange(t.Context(), []byte("{}"), testCred, soon())
	require.NoError(t, err, "the first owner keeps serving")
}

func TestControl_BoundButNotListeningHoldsTheName(t *testing.T) {
	name := controlName(t)
	fd, err := unix.Socket(unix.AF_UNIX, unix.SOCK_STREAM|unix.SOCK_CLOEXEC, 0)
	require.NoError(t, err)
	require.NoError(t, unix.Bind(fd, &unix.SockaddrUnix{Name: "@" + name}))

	_, err = ListenControl(name)
	require.ErrorIs(t, err, unix.EADDRINUSE)

	require.NoError(t, unix.Close(fd))
	ln, err := ListenControl(name)
	require.NoError(t, err)
	require.NoError(t, ln.Close())
}

func TestControl_RestartAfterOwnerClosesNeedsNoCleanup(t *testing.T) {
	name := controlName(t)
	_, stop := serveControl(t, name, os.Getuid(), nil)
	stop()

	serveControl(t, name, os.Getuid(), nil)
	_, err := client(name, os.Getuid()).Exchange(t.Context(), []byte("{}"), testCred, soon())
	require.NoError(t, err)
}

func TestControl_DuplicatedDescriptorHoldsTheName(t *testing.T) {
	name := controlName(t)
	ln, err := ListenControl(name)
	require.NoError(t, err)
	dup, err := ln.File()
	require.NoError(t, err)
	require.NoError(t, ln.Close())

	_, err = ListenControl(name)
	require.ErrorIs(t, err, unix.EADDRINUSE)

	require.NoError(t, dup.Close())
	again, err := ListenControl(name)
	require.NoError(t, err)
	require.NoError(t, again.Close())
}

func TestControl_ChildDoesNotInheritTheListener(t *testing.T) {
	name := controlName(t)
	ln, err := ListenControl(name)
	require.NoError(t, err)

	child := exec.Command("cat")
	stdin, err := child.StdinPipe()
	require.NoError(t, err)
	require.NoError(t, child.Start())
	t.Cleanup(func() { _ = stdin.Close(); _ = child.Wait() })

	require.NoError(t, ln.Close())
	again, err := ListenControl(name)
	require.NoError(t, err, "a live child holds no reference to the name")
	require.NoError(t, again.Close())
}

func TestControl_ClientWithUnexpectedUIDIsClosedUnread(t *testing.T) {
	name := controlName(t)
	s, _ := serveControl(t, name, os.Getuid()+1, nil)

	_, err := client(name, os.Getuid()).Exchange(t.Context(), []byte("{}"), testCred, soon())
	require.Error(t, err)
	assert.Equal(t, int32(0), s.calls.Load())
}

func TestControl_ListenerWithUnexpectedUIDReceivesNothing(t *testing.T) {
	name := controlName(t)
	ln, err := net.ListenUnix("unix", &net.UnixAddr{Name: "@" + name, Net: "unix"})
	require.NoError(t, err)
	t.Cleanup(func() { _ = ln.Close() })
	got := make(chan int, 1)
	go func() {
		c, err := ln.Accept()
		if err != nil {
			got <- -1
			return
		}
		b, _ := io.ReadAll(c)
		got <- len(b)
	}()

	_, err = client(name, os.Getuid()+1).Exchange(t.Context(), []byte("{}"), testCred, soon())
	require.ErrorIs(t, err, ErrNotSent)
	require.ErrorIs(t, err, ErrPeer)
	assert.Equal(t, 0, <-got)
}

func rawFrame(b []byte) []byte {
	var n [4]byte
	binary.BigEndian.PutUint32(n[:], uint32(len(b)))
	return append(n[:], b...)
}

func TestControl_BadFramesAreRefused(t *testing.T) {
	cases := map[string][]byte{
		"oversized request":    rawFrame(bytes.Repeat([]byte("x"), testRequestLimit+1)),
		"empty request":        rawFrame(nil),
		"truncated request":    rawFrame([]byte("{}"))[:4],
		"missing credential":   rawFrame([]byte("{}")),
		"malformed credential": append(rawFrame([]byte("{}")), rawFrame([]byte("not json"))...),
		"incomplete credential": append(rawFrame([]byte("{}")),
			rawFrame([]byte(`{"access_key_id":"ASIA"}`))...),
		"oversized credential": append(rawFrame([]byte("{}")),
			rawFrame(bytes.Repeat([]byte("c"), MaxCredentialBytes+1))...),
	}
	for name, frame := range cases {
		t.Run(name, func(t *testing.T) {
			sock := controlName(t)
			s, _ := serveControl(t, sock, os.Getuid(), nil)
			c, err := net.Dial("unix", "@"+sock)
			require.NoError(t, err)
			_, err = c.Write(frame)
			require.NoError(t, err)
			require.NoError(t, c.(*net.UnixConn).CloseWrite())
			_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
			b, _ := io.ReadAll(c)
			_ = c.Close()
			assert.Empty(t, b, "no reply to a bad frame")
			assert.Equal(t, int32(0), s.calls.Load())
		})
	}
}

// Once the request may have reached the daemon, a lost connection is unknown.
func TestControl_ConnectionLostAfterWriteIsUnknown(t *testing.T) {
	name := controlName(t)
	ln, err := net.ListenUnix("unix", &net.UnixAddr{Name: "@" + name, Net: "unix"})
	require.NoError(t, err)
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		_, _ = readFrame(c, testRequestLimit)
		_, _ = readFrame(c, MaxCredentialBytes)
		_ = c.Close()
	}()

	_, err = client(name, os.Getuid()).Exchange(t.Context(), []byte("{}"), testCred, soon())
	require.Error(t, err)
	assert.False(t, errors.Is(err, ErrNotSent))
}

func TestControl_DialFailureIsNotSent(t *testing.T) {
	_, err := client(controlName(t), os.Getuid()).Exchange(t.Context(), []byte("{}"), testCred, soon())
	require.ErrorIs(t, err, ErrNotSent)
}

// silentPeer accepts one connection, reads `prefix` bytes (or both whole
// frames when prefix is 0), signals, then neither reads nor replies until the
// test ends.
func silentPeer(t *testing.T, name string, prefix int) <-chan struct{} {
	t.Helper()
	ln, err := net.ListenUnix("unix", &net.UnixAddr{Name: "@" + name, Net: "unix"})
	require.NoError(t, err)
	hold, seen := make(chan struct{}), make(chan struct{})
	t.Cleanup(func() { close(hold); _ = ln.Close() })
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		defer func() { _ = c.Close() }()
		if prefix > 0 {
			if _, err := io.ReadFull(c, make([]byte, prefix)); err != nil {
				return
			}
		} else {
			if _, err := readFrame(c, 64<<20); err != nil {
				return
			}
			if _, err := readFrame(c, MaxCredentialBytes); err != nil {
				return
			}
		}
		close(seen)
		<-hold
	}()
	return seen
}

func TestControl_SilentPeerIsUnknownAtTheReplyBound(t *testing.T) {
	name := controlName(t)
	seen := silentPeer(t, name, 0)
	c := client(name, os.Getuid())
	c.IOTimeout = time.Hour

	start := time.Now()
	_, err := c.Exchange(context.Background(), []byte("{}"), testCred, time.Now().Add(300*time.Millisecond))
	require.Error(t, err)
	select {
	case <-seen:
	default:
		t.Fatal("the peer never received both frames, so this is not a reply timeout")
	}
	assert.False(t, errors.Is(err, ErrNotSent), "the request was delivered")
	assert.Less(t, time.Since(start), 5*time.Second)
}

// The reply bound also caps the write, so a slow write cannot dispatch after it.
func TestControl_ReplyBoundCapsTheWrite(t *testing.T) {
	name := controlName(t)
	seen := silentPeer(t, name, 64)
	c := client(name, os.Getuid())
	c.IOTimeout, c.RequestLimit = time.Hour, 32<<20

	start := time.Now()
	_, err := c.Exchange(context.Background(), bytes.Repeat([]byte("x"), 16<<20), testCred, time.Now().Add(300*time.Millisecond))
	require.Error(t, err)
	<-seen
	assert.False(t, errors.Is(err, ErrNotSent), "part of the request was delivered")
	assert.Less(t, time.Since(start), 5*time.Second)
}

func TestControl_ZeroOrPastReplyBoundSendsNothing(t *testing.T) {
	name := controlName(t)
	s, _ := serveControl(t, name, os.Getuid(), nil)
	for label, by := range map[string]time.Time{"zero": {}, "past": time.Now().Add(-time.Second)} {
		_, err := client(name, os.Getuid()).Exchange(t.Context(), []byte("{}"), testCred, by)
		require.ErrorIs(t, err, ErrNotSent, label)
	}
	assert.Equal(t, int32(0), s.calls.Load())
}

// Cancellation ends a write the peer has stopped reading, however long IOTimeout is.
func TestControl_CancelEndsABlockedWrite(t *testing.T) {
	name := controlName(t)
	seen := silentPeer(t, name, 64)
	c := client(name, os.Getuid())
	c.IOTimeout, c.RequestLimit = time.Hour, 32<<20

	ctx, cancel := context.WithCancel(context.Background())
	go func() { <-seen; cancel() }()
	start := time.Now()
	_, err := c.Exchange(ctx, bytes.Repeat([]byte("x"), 16<<20), testCred, time.Now().Add(time.Hour))
	require.Error(t, err)
	assert.ErrorIs(t, ctx.Err(), context.Canceled, "the write ended by cancellation after the peer saw a prefix")
	assert.False(t, errors.Is(err, ErrNotSent), "part of the request was delivered")
	assert.Less(t, time.Since(start), 5*time.Second)
}

func TestControl_CancelledBeforeWriteSendsNothing(t *testing.T) {
	name := controlName(t)
	s, _ := serveControl(t, name, os.Getuid(), nil)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := client(name, os.Getuid()).Exchange(ctx, []byte("{}"), testCred, soon())
	require.ErrorIs(t, err, ErrNotSent)
	assert.Equal(t, int32(0), s.calls.Load())
}
