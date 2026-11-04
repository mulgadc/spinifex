//go:build lambdatestlink

package hostlink

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"net"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/goleak"
)

// fakeConn is a net.Conn that records what is written and the deadlines set,
// and can fail or block on demand.
type fakeConn struct {
	mu        sync.Mutex
	buf       bytes.Buffer
	deadlines []time.Time
	maxWrite  int

	failDeadline func(n int, t time.Time) error
	blockWrites  bool
	started      chan struct{}
	unblock      chan struct{}
	closedCh     chan struct{}
	startOnce    sync.Once
	unblockOnce  sync.Once
	closeOnce    sync.Once
	closes       atomic.Int32
}

var _ net.Conn = (*fakeConn)(nil)

func newFakeConn() *fakeConn {
	return &fakeConn{started: make(chan struct{}), unblock: make(chan struct{}), closedCh: make(chan struct{})}
}

func (c *fakeConn) Read([]byte) (int, error) { <-c.closedCh; return 0, io.EOF }

func (c *fakeConn) Write(p []byte) (int, error) {
	if c.blockWrites {
		c.startOnce.Do(func() { close(c.started) })
		select {
		case <-c.unblock:
			return 0, os.ErrDeadlineExceeded
		case <-c.closedCh:
			return 0, net.ErrClosed
		}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.maxWrite = max(c.maxWrite, len(p))
	return c.buf.Write(p)
}

func (c *fakeConn) Close() error {
	c.closes.Add(1)
	c.closeOnce.Do(func() { close(c.closedCh) })
	return nil
}

func (c *fakeConn) SetWriteDeadline(t time.Time) error {
	c.mu.Lock()
	c.deadlines = append(c.deadlines, t)
	n := len(c.deadlines)
	c.mu.Unlock()
	if c.failDeadline != nil {
		if err := c.failDeadline(n, t); err != nil {
			return err
		}
	}
	if !t.IsZero() && !t.After(time.Now()) {
		c.unblockOnce.Do(func() { close(c.unblock) })
	}
	return nil
}

func (c *fakeConn) LocalAddr() net.Addr             { return nil }
func (c *fakeConn) RemoteAddr() net.Addr            { return nil }
func (c *fakeConn) SetDeadline(time.Time) error     { return nil }
func (c *fakeConn) SetReadDeadline(time.Time) error { return nil }

func (c *fakeConn) written() []byte {
	c.mu.Lock()
	defer c.mu.Unlock()
	return bytes.Clone(c.buf.Bytes())
}

func (c *fakeConn) deadlineLog() []time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]time.Time(nil), c.deadlines...)
}

// pattern returns n deterministic bytes.
func pattern(n int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(i*7 + i/251)
	}
	return b
}

func split(b []byte, seg int) [][]byte {
	out := [][]byte{}
	for len(b) > 0 {
		n := min(seg, len(b))
		out = append(out, b[:n])
		b = b[n:]
	}
	return out
}

func segmented(b []byte, n int) [][]byte { return split(b, n) }

func joined(r Response) []byte {
	if r.Segments != nil {
		return append([]byte{}, bytes.Join(r.Segments, nil)...)
	}
	return r.Payload
}

// assertTodaysFrame checks the written bytes are exactly the length prefix
// and the json.Marshal of the frame, and that readFrame decodes it.
func assertTodaysFrame(t *testing.T, got []byte, r Response) {
	t.Helper()
	plain := r
	plain.Segments = nil
	plain.Payload = joined(r)
	want, err := json.Marshal(frame{Type: typeResult, Response: &plain})
	require.NoError(t, err)
	require.GreaterOrEqual(t, len(got), 4)
	assert.Equal(t, uint32(len(got)-4), binary.BigEndian.Uint32(got[:4]), "length prefix must match the bytes after it")
	if !assert.True(t, bytes.Equal(want, got[4:]), "frame differs from json.Marshal (%d vs %d bytes)", len(want), len(got)-4) {
		return
	}
	f, err := readFrame(bytes.NewReader(got))
	require.NoError(t, err)
	var wf frame
	require.NoError(t, json.Unmarshal(want, &wf))
	assert.Equal(t, *wf.Response, *f.Response)
}

func complete(t *testing.T, r Response) []byte {
	t.Helper()
	c := newFakeConn()
	defer c.Close()
	require.NoError(t, NewTestStream(c).Complete(context.Background(), r))
	return c.written()
}

func TestCompleteMatchesJSONMarshal(t *testing.T) {
	meta := Response{GrantID: "g-1", AttemptID: "a-1", RequestID: "r-1"}
	t.Run("6 MiB from one buffer and from many segments", func(t *testing.T) {
		p := pattern(6 << 20)
		one := meta
		one.Payload = p
		assertTodaysFrame(t, complete(t, one), one)
		many := meta
		many.Segments = segmented(p, 256<<10)
		assertTodaysFrame(t, complete(t, many), many)
		odd := meta
		odd.Segments = segmented(p, 256<<10+1)
		odd.FunctionError = "Unhandled"
		assertTodaysFrame(t, complete(t, odd), odd)
	})
	t.Run("segment lengths against totals of each length mod 3", func(t *testing.T) {
		for _, seg := range []int{1, 2, 3, 4, 5, 256<<10 + 1} {
			totals := []int{0, 1, 2, 3, 4, 5, 6, 7, 64, 65, 66}
			if seg > 5 {
				totals = append(totals, seg-1, seg, seg+1, 2*seg+1, 2*seg+2)
			}
			for _, total := range totals {
				r := meta
				r.Segments = segmented(pattern(total), seg)
				assertTodaysFrame(t, complete(t, r), r)
			}
		}
	})
	t.Run("nil and empty payloads", func(t *testing.T) {
		nilp := meta
		got := complete(t, nilp)
		assert.Contains(t, string(got[4:]), `"payload":null`)
		assertTodaysFrame(t, got, nilp)

		empty := meta
		empty.Payload = []byte{}
		got = complete(t, empty)
		assert.Contains(t, string(got[4:]), `"payload":""`)
		assertTodaysFrame(t, got, empty)

		noSegs := meta
		noSegs.Segments = [][]byte{}
		assertTodaysFrame(t, complete(t, noSegs), noSegs)
	})
	t.Run("metadata is escaped as json.Marshal escapes it", func(t *testing.T) {
		for _, s := range []string{
			`quo"te`, `back\slash`, "ctl\x00\x01\x1f\n\t\r", "naïve 日本語 😀", "<script>&amp;</script>",
			`","payload":"x"`, `{"type":"invoke"}`, `"payload":`, "bad\xffutf8", ``,
		} {
			r := Response{GrantID: s, AttemptID: s + "a", RequestID: "r" + s, Payload: []byte("p"), FunctionError: s}
			assertTodaysFrame(t, complete(t, r), r)
			r.FunctionError = ""
			r.Segments = [][]byte{[]byte("a"), []byte("bc")}
			assertTodaysFrame(t, complete(t, r), r)
		}
	})
	t.Run("a payload landing exactly on maxFrame", func(t *testing.T) {
		for pad := range 4 {
			r := Response{GrantID: "g", AttemptID: "a", RequestID: strings.Repeat("r", 1+pad)}
			rf, err := newResultFrame(r)
			require.NoError(t, err)
			room := maxFrame - rf.size + 2
			if room%4 != 0 {
				continue
			}
			r.Payload = pattern(room / 4 * 3)
			rf, err = newResultFrame(r)
			require.NoError(t, err)
			require.Equal(t, maxFrame, rf.size)
			assertTodaysFrame(t, complete(t, r), r)
			return
		}
		t.Fatal("no request id length lands on maxFrame")
	})
}

func TestCompleteRefusesOverMaxFrameBeforeWriting(t *testing.T) {
	c := newFakeConn()
	defer c.Close()
	s := NewTestStream(c)
	err := s.Complete(context.Background(), Response{Segments: segmented(make([]byte, maxFrame), 1<<20)})
	require.ErrorIs(t, err, ErrFrameTooLarge)
	assert.Empty(t, c.written())
	assert.Empty(t, c.deadlineLog(), "nothing is touched before the size check")
}

// plainRW has no deadline support at all.
type plainRW struct {
	io.Reader
	wrote atomic.Int32
}

func (p *plainRW) Write(b []byte) (int, error) { p.wrote.Add(1); return len(b), nil }

// deadlineRW has SetWriteDeadline but is not a net.Conn.
type deadlineRW struct{ plainRW }

func (*deadlineRW) SetWriteDeadline(time.Time) error { return nil }

func TestCompleteRefusesNonNetConn(t *testing.T) {
	p := &plainRW{Reader: strings.NewReader("")}
	d := &deadlineRW{plainRW{Reader: strings.NewReader("")}}
	for name, tc := range map[string]struct {
		rw    io.ReadWriter
		wrote *atomic.Int32
	}{
		"plain":                 {p, &p.wrote},
		"with SetWriteDeadline": {d, &d.wrote},
	} {
		t.Run(name, func(t *testing.T) {
			s := NewTestStream(tc.rw)
			err := s.Complete(context.Background(), Response{Payload: []byte{1}})
			require.ErrorIs(t, err, ErrNoDeadlineTransport)
			assert.Zero(t, tc.wrote.Load(), "no byte may be written")
		})
	}
}

// sigCtx reports the first time something selects on its Done channel.
type sigCtx struct {
	context.Context
	waiting chan struct{}
	once    sync.Once
}

func (c *sigCtx) Done() <-chan struct{} {
	c.once.Do(func() { close(c.waiting) })
	return c.Context.Done()
}

func TestCompleteCancelledWhileAnotherWriteHoldsTheSlot(t *testing.T) {
	defer goleak.VerifyNone(t, goleak.IgnoreCurrent())
	c := newFakeConn()
	s := NewTestStream(c)
	require.NoError(t, s.acquire(context.Background())) // another writer holds the slot

	base, cancel := context.WithCancel(context.Background())
	ctx := &sigCtx{Context: base, waiting: make(chan struct{})}
	done := make(chan error, 1)
	go func() { done <- s.Complete(ctx, Response{Payload: []byte("x")}) }()
	<-ctx.waiting
	cancel()
	require.ErrorIs(t, <-done, context.Canceled)
	assert.Empty(t, c.written(), "the peer receives no bytes")
	assert.Empty(t, c.deadlineLog())

	s.release()
	require.NoError(t, s.Complete(context.Background(), Response{Payload: []byte("x")}), "the slot is still usable")
	c.Close()
}

func TestCompletePreCancelledReleasesSlot(t *testing.T) {
	c := newFakeConn()
	defer c.Close()
	s := NewTestStream(c)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	require.ErrorIs(t, s.Complete(ctx, Response{Payload: []byte("x")}), context.Canceled)
	assert.Empty(t, c.written())
	assert.Zero(t, len(s.wslot), "slot released")
}

func TestCompleteInitialDeadlineFailureClosesAndWritesNothing(t *testing.T) {
	c := newFakeConn()
	c.failDeadline = func(int, time.Time) error { return errors.New("no deadlines") }
	s := NewTestStream(c)
	err := s.Complete(context.Background(), Response{Payload: []byte("x")})
	require.ErrorContains(t, err, "set write deadline")
	assert.Equal(t, int32(1), c.closes.Load())
	assert.Empty(t, c.written())
	assert.Zero(t, len(s.wslot))
}

func TestCompleteInterruptFailureClosesAndReportsLinkError(t *testing.T) {
	defer goleak.VerifyNone(t, goleak.IgnoreCurrent())
	c := newFakeConn()
	c.blockWrites = true
	c.failDeadline = func(_ int, t time.Time) error {
		if !t.IsZero() && t.Before(time.Now()) {
			return errors.New("cannot interrupt")
		}
		return nil
	}
	s := NewTestStream(c)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.Complete(ctx, Response{Payload: []byte("x")}) }()
	<-c.started
	cancel()
	var err error
	select {
	case err = <-done:
	case <-time.After(5 * time.Second):
		c.Close()
		t.Fatal("Complete stayed blocked: the watcher did not close the connection")
	}
	require.ErrorContains(t, err, "interrupt blocked write")
	assert.Equal(t, int32(1), c.closes.Load())
	assert.Zero(t, len(s.wslot))
}

func TestCompleteClearFailureAfterWriteClosesButFrameWasSent(t *testing.T) {
	c := newFakeConn()
	c.failDeadline = func(n int, _ time.Time) error {
		if n == 2 {
			return errors.New("cannot clear")
		}
		return nil
	}
	s := NewTestStream(c)
	r := Response{GrantID: "g", AttemptID: "a", RequestID: "r", Payload: []byte("done")}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	err := s.Complete(ctx, r)
	require.ErrorContains(t, err, "clear write deadline")
	assert.Equal(t, int32(1), c.closes.Load())
	// The host already holds a complete valid frame and may accept it.
	assertTodaysFrame(t, c.written(), r)
}

func TestCompleteReleasesSlotOnlyAfterDeadlineCleared(t *testing.T) {
	c := newFakeConn()
	clearing, proceed := make(chan struct{}), make(chan struct{})
	c.failDeadline = func(n int, t time.Time) error {
		if n == 2 { // writer A clearing its deadline
			close(clearing)
			<-proceed
		}
		return nil
	}
	s := NewTestStream(c)
	ctxA, cancelA := context.WithTimeout(context.Background(), time.Minute)
	defer cancelA()
	aDone := make(chan error, 1)
	go func() { aDone <- s.Complete(ctxA, Response{Payload: []byte("a")}) }()
	<-clearing

	base, cancelB := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancelB()
	ctxB := &sigCtx{Context: base, waiting: make(chan struct{})}
	bDone := make(chan error, 1)
	go func() { bDone <- s.Complete(ctxB, Response{Payload: []byte("b")}) }()
	<-ctxB.waiting
	assert.Len(t, c.deadlineLog(), 2, "B has set no deadline while A is still cleaning up")

	close(proceed)
	require.NoError(t, <-aDone)
	require.NoError(t, <-bDone)
	log := c.deadlineLog()
	require.Len(t, log, 4)
	assert.False(t, log[0].IsZero())
	assert.True(t, log[1].IsZero(), "A clears first")
	assert.False(t, log[2].IsZero())
	assert.True(t, log[3].IsZero())
	c.Close()
}

func TestCompleteStopsAtDeadlineWhenPeerNeverReads(t *testing.T) {
	g, h := net.Pipe()
	defer h.Close()
	s := NewTestStream(g)
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	err := s.Complete(ctx, Response{GrantID: "g", Payload: []byte("x")})
	require.Error(t, err)
	assert.ErrorIs(t, err, os.ErrDeadlineExceeded)
	assert.Zero(t, len(s.wslot))

	// The host never received a frame, so it has nothing to accept.
	host := NewTestStream(h)
	g.Close()
	_, err = host.HostResult(context.Background())
	require.Error(t, err)
}

func TestCompleteCancelInterruptsBlockedWrite(t *testing.T) {
	defer goleak.VerifyNone(t, goleak.IgnoreCurrent())
	c := newFakeConn()
	c.blockWrites = true
	s := NewTestStream(c)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.Complete(ctx, Response{Payload: []byte("x")}) }()
	<-c.started
	cancel()
	require.ErrorIs(t, <-done, os.ErrDeadlineExceeded)
	assert.Equal(t, int32(1), c.closes.Load(), "a frame may be half sent, so the connection is closed")
	assert.ErrorIs(t, s.Complete(context.Background(), Response{Payload: []byte("x")}), ErrStreamClosed)
}

func TestCompleteCancelAfterWriteDoesNotTouchConn(t *testing.T) {
	defer goleak.VerifyNone(t, goleak.IgnoreCurrent())
	c := newFakeConn()
	s := NewTestStream(c)
	ctx, cancel := context.WithCancel(context.Background())
	require.NoError(t, s.Complete(ctx, Response{Payload: []byte("x")}))
	before := len(c.deadlineLog())
	cancel()
	assert.Len(t, c.deadlineLog(), before, "no watcher is left to move the deadline")
	assert.Zero(t, c.closes.Load())
	c.Close()
}

func TestCompleteWritesNeverInterleave(t *testing.T) {
	c := newFakeConn()
	defer c.Close()
	s := NewTestStream(c)
	const n = 16
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if i%2 == 0 {
				assert.NoError(t, s.Complete(context.Background(), Response{GrantID: "g", Segments: segmented(pattern(100000+i), 7001)}))
			} else {
				assert.NoError(t, s.HostSend(Request{GrantID: "g", Payload: pattern(100000 + i)}))
			}
		}()
	}
	wg.Wait()
	r := bytes.NewReader(c.written())
	var results, invokes int
	for r.Len() > 0 {
		f, err := readFrame(r)
		require.NoError(t, err)
		switch f.Type {
		case typeResult:
			results++
		case typeInvoke:
			invokes++
		}
	}
	assert.Equal(t, n/2, results)
	assert.Equal(t, n/2, invokes)
}

// discardConn drops writes so a benchmark does not hold the frame.
type discardConn struct{ *fakeConn }

func (discardConn) Write(p []byte) (int, error) { return len(p), nil }

func TestCompleteMakesNoPayloadSizedAllocation(t *testing.T) {
	r := Response{GrantID: "g", AttemptID: "a", RequestID: "r", Segments: segmented(pattern(6<<20), 256<<10)}
	c := newFakeConn()
	defer c.Close()
	require.NoError(t, NewTestStream(c).Complete(context.Background(), r))
	assert.Less(t, c.maxWrite, 64<<10, "no single write is payload sized")

	d := discardConn{newFakeConn()}
	defer d.Close()
	bs := NewTestStream(d)
	br := testing.Benchmark(func(b *testing.B) {
		b.ReportAllocs()
		for range b.N {
			if err := bs.Complete(context.Background(), r); err != nil {
				b.Fatal(err)
			}
		}
	})
	assert.Less(t, br.AllocedBytesPerOp(), int64(1<<20), "writing a 6 MiB result")
}

func TestResponseJSONCompatibility(t *testing.T) {
	plain := Response{GrantID: "g", AttemptID: "a", RequestID: "r", Payload: []byte("p<&>"), FunctionError: "Unhandled"}

	t.Run("nil segments encode as the plain struct", func(t *testing.T) {
		got, err := json.Marshal(plain)
		require.NoError(t, err)
		want := `{"grant_id":"g","attempt_id":"a","request_id":"r","payload":"cDwmPg==","function_error":"Unhandled"}`
		assert.Equal(t, want, string(got))
		inFrame, err := json.Marshal(frame{Type: typeResult, Response: &plain})
		require.NoError(t, err)
		assert.Equal(t, `{"type":"result","response":`+want+`}`, string(inFrame))
		nilPayload, err := json.Marshal(Response{GrantID: "g"})
		require.NoError(t, err)
		assert.Equal(t, `{"grant_id":"g","attempt_id":"","request_id":"","payload":null}`, string(nilPayload))
	})
	t.Run("a legacy frame decodes as before", func(t *testing.T) {
		legacy, err := json.Marshal(frame{Type: typeResult, Response: &plain})
		require.NoError(t, err)
		var f frame
		require.NoError(t, json.Unmarshal(legacy, &f))
		assert.Equal(t, plain, *f.Response)
	})
	t.Run("unknown fields including segments are ignored", func(t *testing.T) {
		var f frame
		in := `{"type":"result","extra":1,"response":{"grant_id":"g","segments":{"x":1},"unknown":[1],"payload":"cA=="}}`
		require.NoError(t, json.Unmarshal([]byte(in), &f))
		assert.Equal(t, Response{GrantID: "g", Payload: []byte("p")}, *f.Response)
	})
	t.Run("a segmented payload cannot be marshalled", func(t *testing.T) {
		r := Response{GrantID: "g", Segments: segmented([]byte("data"), 2)}
		_, err := json.Marshal(r)
		require.ErrorIs(t, err, ErrSegmentedNotMarshalable)
		_, err = json.Marshal(frame{Type: typeResult, Response: &r})
		require.ErrorIs(t, err, ErrSegmentedNotMarshalable)
		_, err = json.Marshal(Response{GrantID: "g", Segments: [][]byte{}})
		require.ErrorIs(t, err, ErrSegmentedNotMarshalable)
	})
}

// partialConn accepts n bytes of a write and then fails.
type partialConn struct {
	*fakeConn
	budget int
}

func (p *partialConn) Write(b []byte) (int, error) {
	if p.budget < len(b) {
		n := p.budget
		p.budget = 0
		p.fakeConn.Write(b[:n])
		return n, errors.New("link broke")
	}
	p.budget -= len(b)
	return p.fakeConn.Write(b)
}

func TestCompleteAfterPartialWriteSendsNothingMore(t *testing.T) {
	pc := &partialConn{fakeConn: newFakeConn(), budget: 20}
	s := NewTestStream(pc)
	err := s.Complete(context.Background(), Response{GrantID: "g", Payload: pattern(100)})
	require.ErrorContains(t, err, "link broke")
	assert.Equal(t, int32(1), pc.closes.Load(), "closed while the slot was held")
	assert.Zero(t, len(s.wslot))
	partial := len(pc.written())
	assert.Less(t, partial, 4+100, "the host holds an incomplete frame")

	pc.budget = 1 << 20
	require.ErrorIs(t, s.Complete(context.Background(), Response{GrantID: "g2", Payload: []byte("again")}), ErrStreamClosed)
	assert.Len(t, pc.written(), partial, "the peer receives no further bytes")
	_, err = readFrame(bytes.NewReader(pc.written()))
	assert.Error(t, err, "the host has no valid frame to accept")
}
