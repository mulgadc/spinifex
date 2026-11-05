//go:build lambdatestlink

//test:in-package — reads frames with the unexported readFrame and drives ControlServer limits directly
package testlink

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"net"
	"os"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"

	"github.com/mulgadc/spinifex/spinifex/agents/lambda/hostlink"
)

// fullServe runs a ControlServer at the declared bounds on ln and returns a
// cancel that waits for Serve.
func fullServe(t *testing.T, ln *net.UnixListener, handle func(context.Context, []byte, Credential) Reply) func() {
	t.Helper()
	srv := &ControlServer{GatewayUID: os.Getuid(), RequestLimit: MaxFrameBytes, ReplyLimit: MaxFrameBytes, IOTimeout: 60 * time.Second, Handle: handle}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- srv.Serve(ctx, ln) }()
	var once sync.Once
	stop := func() {
		once.Do(func() {
			cancel()
			select {
			case err := <-done:
				require.NoError(t, err, "Serve returned an error on shutdown")
			case <-time.After(20 * time.Second):
				t.Fatal("Serve did not return")
			}
		})
	}
	t.Cleanup(stop)
	return stop
}

func fullListen(t *testing.T, name string) *net.UnixListener {
	t.Helper()
	ln, err := ListenControl(name)
	require.NoError(t, err)
	return ln
}

func fullClient(name string) *ControlClient {
	return &ControlClient{Name: name, DaemonUID: os.Getuid(), RequestLimit: MaxFrameBytes, ReplyLimit: MaxFrameBytes, IOTimeout: 60 * time.Second}
}

// requestOfSize returns an encoded Request with a payload of payloadLen and
// a FunctionARN padded so the frame is exactly size bytes.
func requestOfSize(t *testing.T, payloadLen, size int) []byte {
	t.Helper()
	req := Request{AttemptID: "0123456789ab", Payload: bytes.Repeat([]byte{7}, payloadLen)}
	b, err := json.Marshal(req)
	require.NoError(t, err)
	req.FunctionARN = "a"
	b, err = json.Marshal(req)
	require.NoError(t, err)
	require.LessOrEqual(t, len(b), size)
	req.FunctionARN = strings.Repeat("a", 1+size-len(b))
	b, err = json.Marshal(req)
	require.NoError(t, err)
	require.Len(t, b, size)
	return b
}

// replyOfSize returns a Reply with a payload of payloadLen whose encoding is
// exactly size bytes.
func replyOfSize(t *testing.T, payloadLen, size int) Reply {
	t.Helper()
	rep := Reply{Outcome: OutcomeDone}
	if payloadLen > 0 {
		rep.Payload = bytes.Repeat([]byte{9}, payloadLen)
	}
	b, err := json.Marshal(rep)
	require.NoError(t, err)
	rep.Error = "e"
	b, err = json.Marshal(rep)
	require.NoError(t, err)
	require.LessOrEqual(t, len(b), size)
	rep.Error = strings.Repeat("e", 1+size-len(b))
	b, err = json.Marshal(rep)
	require.NoError(t, err)
	require.Len(t, b, size)
	return rep
}

func recvBounded[T any](t *testing.T, ch chan T) T {
	t.Helper()
	select {
	case v := <-ch:
		return v
	case <-time.After(30 * time.Second):
		t.Fatal("no value within 30 s")
		var zero T
		return zero
	}
}

func allocatedDuring(f func()) uint64 {
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	f()
	runtime.ReadMemStats(&after)
	return after.TotalAlloc - before.TotalAlloc
}

// Test 5: a frame over MaxFrameBytes is rejected on its header, before the
// receiver allocates its body.
func TestReadFrame_OverBoundRejectedBeforeAllocation(t *testing.T) {
	var hdr [4]byte
	binary.BigEndian.PutUint32(hdr[:], uint32(MaxFrameBytes+1))
	var err error
	alloc := allocatedDuring(func() { _, err = readFrame(bytes.NewReader(hdr[:]), MaxFrameBytes) })
	require.ErrorContains(t, err, "outside 1..")
	require.Less(t, alloc, uint64(1<<20), "no body buffer was allocated")
}

// Test 5: a direct peer's frame with a small payload and metadata far over
// 64 KiB, within MaxFrameBytes, reaches Handle: metadata has no separate bound.
func TestControl_LargeMetadataWithinTheFrameIsAccepted(t *testing.T) {
	name := controlName(t)
	got := make(chan []byte, 1)
	fullServe(t, fullListen(t, name), func(_ context.Context, req []byte, _ Credential) Reply {
		got <- req
		return Reply{Outcome: OutcomeDone}
	})
	req := requestOfSize(t, 16, 1<<20)
	_, err := fullClient(name).Exchange(context.Background(), req, testCred, time.Now().Add(30*time.Second))
	require.NoError(t, err)
	require.Equal(t, req, recvBounded(t, got))
}

// Test 6a: a Request and a Reply of exactly MaxFrameBytes with a full 6 MiB
// payload are both carried byte-identical.
func TestControl_AtBoundRequestAndReplyBothFit(t *testing.T) {
	name := controlName(t)
	want := replyOfSize(t, MaxPayloadBytes, int(MaxFrameBytes))
	got := make(chan []byte, 1)
	fullServe(t, fullListen(t, name), func(_ context.Context, req []byte, _ Credential) Reply {
		got <- req
		return want
	})
	req := requestOfSize(t, MaxPayloadBytes, int(MaxFrameBytes))
	rep, err := fullClient(name).Exchange(context.Background(), req, testCred, time.Now().Add(60*time.Second))
	require.NoError(t, err)
	require.Equal(t, req, recvBounded(t, got))
	require.Equal(t, want, rep)
}

// Test 6a: an at-bound reply with no payload, whose payload field the encoder
// omits, still fits.
func TestControl_AtBoundReplyWithoutPayloadFits(t *testing.T) {
	name := controlName(t)
	want := replyOfSize(t, 0, int(MaxFrameBytes))
	fullServe(t, fullListen(t, name), func(context.Context, []byte, Credential) Reply { return want })
	rep, err := fullClient(name).Exchange(context.Background(), []byte(`{"attempt_id":"a"}`), testCred, time.Now().Add(60*time.Second))
	require.NoError(t, err)
	require.Equal(t, want, rep)
}

// Test 6b: the real server answers a handler's oversized payload, and a reply
// encoding over MaxFrameBytes, with the small unknown reply.
func TestControl_ServerReplacesOversizedRepliesWithUnknown(t *testing.T) {
	over := Reply{Outcome: OutcomeDone, Payload: make([]byte, MaxPayloadBytes+1)}
	for name, rep := range map[string]Reply{
		"payload over 6 MiB":        over,
		"encoding over frame bound": replyOfSize(t, MaxPayloadBytes, int(MaxFrameBytes)+1),
	} {
		t.Run(name, func(t *testing.T) {
			sock := controlName(t)
			fullServe(t, fullListen(t, sock), func(context.Context, []byte, Credential) Reply { return rep })
			got, err := fullClient(sock).Exchange(context.Background(), []byte(`{"attempt_id":"a"}`), testCred, time.Now().Add(60*time.Second))
			require.NoError(t, err)
			require.Equal(t, Reply{Outcome: OutcomeUnknown, Error: "result too large for the test path"}, got)
		})
	}
	// The oversized payload is refused before the reply is encoded: one
	// exchange allocates far less than an 8 MiB base64 encoding.
	sock := controlName(t)
	fullServe(t, fullListen(t, sock), func(context.Context, []byte, Credential) Reply { return over })
	var err error
	alloc := allocatedDuring(func() {
		_, err = fullClient(sock).Exchange(context.Background(), []byte(`{"attempt_id":"a"}`), testCred, time.Now().Add(60*time.Second))
	})
	require.NoError(t, err)
	require.Less(t, alloc, uint64(4<<20), "the oversized reply was encoded before the payload check")
}

// directPeer serves one connection by reading the request and credential,
// then writing with body; the peer's own error is returned on the channel.
func directPeer(t *testing.T, name string, body func(net.Conn) error) chan error {
	t.Helper()
	ln := fullListen(t, name)
	t.Cleanup(func() { _ = ln.Close() })
	peerErr := make(chan error, 1)
	go func() {
		peerErr <- func() error {
			c, err := ln.AcceptUnix()
			if err != nil {
				return err
			}
			defer func() { _ = c.Close() }()
			if _, err := readFrame(c, MaxFrameBytes); err != nil {
				return err
			}
			if _, err := readFrame(c, MaxCredentialBytes); err != nil {
				return err
			}
			return body(c)
		}()
	}()
	return peerErr
}

// Test 6: replies from a direct peer daemon over the payload bound or the
// frame bound are errors after sending, so the gateway reports unknown.
func TestControl_ClientRejectsOversizedReplies(t *testing.T) {
	overPayload, err := json.Marshal(Reply{Outcome: OutcomeDone, Payload: make([]byte, MaxPayloadBytes+1)})
	require.NoError(t, err)
	require.LessOrEqual(t, int64(len(overPayload)), MaxFrameBytes)
	cases := map[string]struct {
		send func(net.Conn) error
		want string
	}{
		"payload over 6 MiB": {func(c net.Conn) error { return writeFrame(c, overPayload) }, "reply payload of 6291457 bytes exceeds 6291456"},
		"frame over bound": {func(c net.Conn) error {
			var hdr [4]byte
			binary.BigEndian.PutUint32(hdr[:], uint32(MaxFrameBytes+1))
			_, err := c.Write(hdr[:])
			return err
		}, "outside 1.."},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			sock := controlName(t)
			peerErr := directPeer(t, sock, tc.send)
			_, err := fullClient(sock).Exchange(context.Background(), []byte(`{"attempt_id":"a"}`), testCred, time.Now().Add(30*time.Second))
			require.NoError(t, recvBounded(t, peerErr), "the peer read the request and wrote its reply")
			require.ErrorContains(t, err, tc.want)
			require.NotErrorIs(t, err, ErrNotSent)
		})
	}
}

// Test 12: the credential bound is unchanged beside a full-size request.
func TestControl_CredentialOverBoundRefusedWithAFullRequest(t *testing.T) {
	cred := testCred
	b, err := json.Marshal(cred)
	require.NoError(t, err)
	cred.SessionToken += strings.Repeat("t", MaxCredentialBytes+1-len(b))
	b, err = json.Marshal(cred)
	require.NoError(t, err)
	require.Len(t, b, MaxCredentialBytes+1)

	name := controlName(t)
	s, _ := serveControl(t, name, os.Getuid(), nil)
	_, err = fullClient(name).Exchange(context.Background(), requestOfSize(t, MaxPayloadBytes, int(MaxFrameBytes)), cred, time.Now().Add(30*time.Second))
	require.ErrorIs(t, err, ErrNotSent)
	require.Zero(t, s.calls.Load())
}

// pausedHandlers counts concurrent Handle calls and blocks each until released.
type pausedHandlers struct {
	in, max atomic.Int32
	calls   atomic.Int32
	entered chan []byte
	release chan struct{}
	once    sync.Once
}

func newPaused(t *testing.T) *pausedHandlers {
	p := &pausedHandlers{entered: make(chan []byte, 64), release: make(chan struct{})}
	t.Cleanup(p.free)
	return p
}

func (p *pausedHandlers) free() { p.once.Do(func() { close(p.release) }) }

func (p *pausedHandlers) handle(ctx context.Context, req []byte, _ Credential) Reply {
	n := p.in.Add(1)
	for {
		m := p.max.Load()
		if n <= m || p.max.CompareAndSwap(m, n) {
			break
		}
	}
	p.calls.Add(1)
	p.entered <- req
	select {
	case <-p.release:
	case <-ctx.Done():
	}
	p.in.Add(-1)
	return Reply{Outcome: OutcomeDone}
}

func (p *pausedHandlers) waitEntered(t *testing.T, n int) {
	t.Helper()
	for range n {
		select {
		case <-p.entered:
		case <-time.After(30 * time.Second):
			t.Fatal("handler not entered")
		}
	}
}

// serverConns reads the kernel's socket table for the abstract name: server
// ends Accept returned are connected and held by this process; a connection
// still in the listen backlog is connecting, with no inode until accepted.
func serverConns(t *testing.T, name string) (accepted, backlog int) {
	t.Helper()
	held := map[string]bool{}
	fds, err := os.ReadDir("/proc/self/fd")
	require.NoError(t, err)
	for _, fd := range fds {
		if target, err := os.Readlink("/proc/self/fd/" + fd.Name()); err == nil && strings.HasPrefix(target, "socket:[") {
			held[strings.TrimSuffix(strings.TrimPrefix(target, "socket:["), "]")] = true
		}
	}
	table, err := os.ReadFile("/proc/net/unix")
	require.NoError(t, err)
	for _, line := range strings.Split(string(table), "\n")[1:] {
		f := strings.Fields(line)
		// Num RefCount Protocol Flags Type St Inode Path.
		if len(f) != 8 || f[7] != "@"+name {
			continue
		}
		switch {
		case f[5] == "03" && held[f[6]]:
			accepted++
		case f[5] == "02" && f[6] == "0":
			backlog++
		}
	}
	return accepted, backlog
}

// Test 8: eight full-size requests at once; with two admitted connections
// paused in Handle, the kernel shows two accepted and six still in the
// backlog, and at most two Handle calls ever run at once.
func TestControl_AdmissionHoldsAtMostTwoFullSizeRequests(t *testing.T) {
	name := controlName(t)
	p := newPaused(t)
	fullServe(t, fullListen(t, name), p.handle)
	req := requestOfSize(t, MaxPayloadBytes, int(MaxFrameBytes))

	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for range 8 {
		wg.Go(func() {
			_, err := fullClient(name).Exchange(context.Background(), req, testCred, time.Now().Add(90*time.Second))
			errs <- err
		})
	}
	p.waitEntered(t, MaxAdmitted)
	deadline := time.Now().Add(30 * time.Second)
	for {
		accepted, backlog := serverConns(t, name)
		require.LessOrEqual(t, accepted, MaxAdmitted, "a third connection was accepted")
		if backlog == 8-MaxAdmitted {
			break
		}
		require.True(t, time.Now().Before(deadline), "the other clients never reached the backlog: %d accepted, %d waiting", accepted, backlog)
		runtime.Gosched()
	}
	for range 20 {
		accepted, backlog := serverConns(t, name)
		require.Equal(t, MaxAdmitted, accepted, "only the admitted connections are accepted")
		require.Equal(t, 8-MaxAdmitted, backlog, "the other six stay unaccepted")
		<-time.After(50 * time.Millisecond)
	}
	require.Equal(t, int32(MaxAdmitted), p.calls.Load())

	p.free()
	waitGroupBounded(t, &wg, 90*time.Second)
	close(errs)
	for err := range errs {
		require.NoError(t, err)
	}
	require.Equal(t, int32(8), p.calls.Load())
	require.LessOrEqual(t, p.max.Load(), int32(MaxAdmitted))
}

// Test 9: a gateway that wrote its whole request while unaccepted, then gave
// up and closed, reports unknown; the daemon later admits it and Handle runs.
func TestControl_GatewayGivesUpButBufferedRequestIsStillHandled(t *testing.T) {
	name := controlName(t)
	p := newPaused(t)
	fullServe(t, fullListen(t, name), p.handle)
	for range MaxAdmitted {
		go func() {
			_, _ = fullClient(name).Exchange(context.Background(), []byte(`{"attempt_id":"held"}`), testCred, time.Now().Add(60*time.Second))
		}()
	}
	p.waitEntered(t, MaxAdmitted)

	late := []byte(`{"attempt_id":"late"}`)
	c := fullClient(name)
	c.IOTimeout = 2 * time.Second
	_, err := c.Exchange(context.Background(), late, testCred, time.Now().Add(2*time.Second))
	require.Error(t, err)
	require.NotErrorIs(t, err, ErrNotSent, "the request was written, so the outcome is unknown")

	p.free()
	deadline := time.After(30 * time.Second)
	for {
		select {
		case req := <-p.entered:
			if bytes.Equal(req, late) {
				return // Handle ran for a request whose gateway reported unknown.
			}
		case <-deadline:
			t.Fatal("the buffered request was never handled")
		}
	}
}

// smallBacklogListener binds name with a listen backlog of one.
func smallBacklogListener(t *testing.T, name string) *net.UnixListener {
	t.Helper()
	fd, err := unix.Socket(unix.AF_UNIX, unix.SOCK_STREAM|unix.SOCK_CLOEXEC, 0)
	require.NoError(t, err)
	require.NoError(t, unix.Bind(fd, &unix.SockaddrUnix{Name: "@" + name}))
	require.NoError(t, unix.Listen(fd, 1))
	f := os.NewFile(uintptr(fd), name)
	defer func() { _ = f.Close() }()
	l, err := net.FileListener(f)
	require.NoError(t, err)
	return l.(*net.UnixListener)
}

// Test 10: with both slots held and the backlog full, a gateway connect made
// with no context deadline fails or stops at replyBy as ErrNotSent, and no
// request reaches Handle.
func TestControl_FullBacklogConnectIsNotSent(t *testing.T) {
	name := controlName(t)
	p := newPaused(t)
	fullServe(t, smallBacklogListener(t, name), p.handle)
	for range MaxAdmitted {
		go func() {
			_, _ = fullClient(name).Exchange(context.Background(), []byte(`{"attempt_id":"held"}`), testCred, time.Now().Add(60*time.Second))
		}()
	}
	p.waitEntered(t, MaxAdmitted)

	// Fill the backlog with raw connections that never write.
	var fillers []net.Conn
	t.Cleanup(func() {
		for _, c := range fillers {
			_ = c.Close()
		}
	})
	for range 64 {
		d := net.Dialer{Timeout: 200 * time.Millisecond}
		c, err := d.Dial("unix", "@"+name)
		if err != nil {
			break
		}
		fillers = append(fillers, c)
	}
	require.Less(t, len(fillers), 64, "the backlog never filled")

	began := time.Now()
	_, err := fullClient(name).Exchange(context.Background(), []byte(`{"attempt_id":"x"}`), testCred, time.Now().Add(2*time.Second))
	require.ErrorIs(t, err, ErrNotSent)
	require.Less(t, time.Since(began), 10*time.Second)
	require.Equal(t, int32(MaxAdmitted), p.calls.Load(), "no further request reached Handle")
}

// Test 15 (measurement, not a bound): two admitted full-size lifetimes, each
// decoding and fit-checking its request as the daemon does and answering with
// a 6 MiB reply, measured from the first dial until both replies are read.
func TestControl_MeasureTwoAdmittedFullSizeLifetimes(t *testing.T) {
	name := controlName(t)
	var mu sync.Mutex
	entered := 0
	both := make(chan struct{})
	fitErrs := make(chan error, MaxAdmitted)
	fullServe(t, fullListen(t, name), func(ctx context.Context, b []byte, cred Credential) Reply {
		var req Request
		if err := json.Unmarshal(b, &req); err != nil {
			return Reply{Outcome: OutcomeRefused, Error: err.Error()}
		}
		req.cred = cred
		fitErrs <- hostlink.CheckInvokeFits(guestRequest(req, 0))
		mu.Lock()
		entered++
		if entered == MaxAdmitted {
			close(both)
		}
		mu.Unlock()
		select {
		case <-both:
		case <-ctx.Done():
			return Reply{Outcome: OutcomeUnknown}
		}
		return Reply{Outcome: OutcomeDone, Payload: bytes.Repeat([]byte{9}, MaxPayloadBytes)}
	})
	req := requestOfSize(t, MaxPayloadBytes, int(MaxFrameBytes))

	// HeapAlloc is sampled every 2 ms, so its maximum is a sampled figure for
	// the whole process, not a true peak.
	var heapMax uint64
	samples := 0
	stopSampling, sampled := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(sampled)
		tick := time.NewTicker(2 * time.Millisecond)
		defer tick.Stop()
		for {
			var m runtime.MemStats
			runtime.ReadMemStats(&m)
			heapMax, samples = max(heapMax, m.HeapAlloc), samples+1
			select {
			case <-tick.C:
			case <-stopSampling:
				return
			}
		}
	}()
	var wg sync.WaitGroup
	var reps [MaxAdmitted]Reply
	var errs [MaxAdmitted]error
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	for i := range MaxAdmitted {
		wg.Go(func() {
			reps[i], errs[i] = fullClient(name).Exchange(context.Background(), req, testCred, time.Now().Add(60*time.Second))
		})
	}
	waitGroupBounded(t, &wg, 60*time.Second)
	runtime.ReadMemStats(&after)
	close(stopSampling)
	recvBounded(t, sampled)

	for i := range MaxAdmitted {
		require.NoError(t, errs[i])
		require.Equal(t, OutcomeDone, reps[i].Outcome)
		require.Len(t, reps[i].Payload, MaxPayloadBytes, "each lifetime ends with its full-size reply read")
		require.NoError(t, recvBounded(t, fitErrs), "the full-size request fits the guest link")
	}
	// Excludes the executor's marshaling, HostSend, the guest and QEMU.
	t.Logf("two admitted full-size lifetimes, process-wide (client and server): %d bytes in %d allocations; sampled max HeapAlloc %d bytes over %d samples",
		after.TotalAlloc-before.TotalAlloc, after.Mallocs-before.Mallocs, heapMax, samples)
}
