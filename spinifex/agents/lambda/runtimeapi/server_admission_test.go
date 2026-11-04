package runtimeapi

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/goleak"
)

// await bounds a receive so a regression fails promptly instead of hanging.
func await[T any](t *testing.T, ch <-chan T) T {
	t.Helper()
	select {
	case v := <-ch:
		return v
	case <-time.After(15 * time.Second):
		t.Fatal("timed out waiting on a channel")
		panic("unreachable")
	}
}

// arm puts a delivered, unclaimed flight in place without any HTTP traffic.
func arm(s *Server, id string) *flight {
	f := &flight{inv: inv(id, time.Minute), state: stRunning, done: make(chan outcome, 1)}
	s.mu.Lock()
	s.cur = f
	s.mu.Unlock()
	return f
}

func postTo(s *Server, kind, id string, body io.Reader, length int64) *httptest.ResponseRecorder {
	r := httptest.NewRequest(http.MethodPost, base+"invocation/"+id+"/"+kind, body)
	r.ContentLength = length
	r.SetPathValue("id", id)
	rec := httptest.NewRecorder()
	if kind == "response" {
		s.response(rec, r)
	} else {
		s.invocationError(rec, r)
	}
	return rec
}

// patterned yields n bytes of a repeating pattern without holding them.
type patterned struct{ n, off int }

func (p *patterned) Read(b []byte) (int, error) {
	if p.off >= p.n {
		return 0, io.EOF
	}
	c := min(len(b), p.n-p.off)
	for i := range c {
		b[i] = byte((p.off + i) % 251)
	}
	p.off += c
	return c, nil
}

func pattern(n int) []byte {
	b := make([]byte, n)
	_, _ = io.ReadFull(&patterned{n: n}, b)
	return b
}

func concat(r Result) []byte {
	if r.Segments == nil {
		return r.Payload
	}
	return bytes.Join(r.Segments, nil)
}

// counting records whether the handler read the body at all.
type counting struct{ reads atomic.Int32 }

func (c *counting) Read([]byte) (int, error) { c.reads.Add(1); return 0, io.EOF }

// gated blocks its first Read until released and reports that it started.
type gated struct {
	started chan struct{}
	release chan struct{}
	rest    io.Reader
	once    atomic.Bool
}

func (g *gated) Read(b []byte) (int, error) {
	if g.once.CompareAndSwap(false, true) {
		close(g.started)
		<-g.release
	}
	return g.rest.Read(b)
}

func TestOnlyOnePostPerInvocationIsRead(t *testing.T) {
	for _, first := range []string{"response", "error"} {
		for _, second := range []string{"response", "error"} {
			t.Run(first+"_then_"+second, func(t *testing.T) {
				s := New()
				f := arm(s, "req-1")
				g := &gated{started: make(chan struct{}), release: make(chan struct{}), rest: bytes.NewReader([]byte("body"))}
				firstDone := make(chan *httptest.ResponseRecorder, 1)
				go func() { firstDone <- postTo(s, first, "req-1", g, 4) }()
				await(t, g.started)

				c := &counting{}
				rec := postTo(s, second, "req-1", c, 4)
				assert.Equal(t, http.StatusBadRequest, rec.Code)
				assert.Equal(t, "InvalidRequestID", errType(t, rec.Body.Bytes()))
				assert.Zero(t, c.reads.Load(), "the refused POST's body was read")
				assert.Empty(t, f.done, "nothing completes while the admitted body is still arriving")

				close(g.release)
				assert.Equal(t, http.StatusAccepted, (await(t, firstDone)).Code)
				assert.Equal(t, "body", string(concat((await(t, f.done)).res)))
			})
		}
	}
}

func TestDeclaredOversizeCompletesWithoutPayloadBuffer(t *testing.T) {
	s := New()
	f := arm(s, "req-1")
	n := MaxResponseBytes + 10
	rec := postTo(s, "response", "req-1", &patterned{n: n}, int64(n))
	assert.Equal(t, http.StatusRequestEntityTooLarge, rec.Code)
	res := (await(t, f.done)).res
	assert.Equal(t, ErrTypeUnhandled, res.FunctionError)
	assert.Equal(t, ErrTypeResponseTooLarge, errType(t, res.Payload))

	br := testing.Benchmark(func(b *testing.B) {
		b.ReportAllocs()
		for range b.N {
			s := New()
			f := arm(s, "req-1")
			postTo(s, "response", "req-1", &patterned{n: n}, int64(n))
			await(t, f.done)
		}
	})
	assert.Less(t, br.AllocedBytesPerOp(), int64(256<<10), "an oversize body must not be buffered")
}

func TestDeclaredOversizeCutShortIsUnknown(t *testing.T) {
	s := New()
	f := arm(s, "req-1")
	rec := postTo(s, "response", "req-1", &patterned{n: MaxResponseBytes}, MaxResponseBytes+10)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.ErrorIs(t, (await(t, f.done)).err, ErrResponseBodyFailed, "no result is made from a partial body")

	rec = postTo(s, "response", "req-1", &patterned{n: 1}, 1)
	assert.Equal(t, "InvalidRequestID", errType(t, rec.Body.Bytes()), "the slot stays consumed")
}

func TestKnownLengthWithinCapAllocatesOnce(t *testing.T) {
	s := New()
	f := arm(s, "req-1")
	rec := postTo(s, "response", "req-1", &patterned{n: MaxResponseBytes}, MaxResponseBytes)
	assert.Equal(t, http.StatusAccepted, rec.Code)
	res := (await(t, f.done)).res
	assert.Nil(t, res.Segments)
	assert.Equal(t, pattern(MaxResponseBytes), res.Payload)

	s = New()
	f = arm(s, "req-1")
	rec = postTo(s, "response", "req-1", &patterned{n: 100}, 200)
	assert.Equal(t, http.StatusBadRequest, rec.Code, "a body ending short of its length is truncated")
	out := await(t, f.done)
	assert.ErrorIs(t, out.err, ErrResponseBodyFailed)
	assert.Empty(t, out.res.Payload)
}

func TestChunkedBodyCap(t *testing.T) {
	t.Run("exactly the cap is accepted in order", func(t *testing.T) {
		s := New()
		f := arm(s, "req-1")
		rec := postTo(s, "response", "req-1", &patterned{n: MaxResponseBytes}, -1)
		assert.Equal(t, http.StatusAccepted, rec.Code)
		res := (await(t, f.done)).res
		assert.Empty(t, res.FunctionError)
		require.NotNil(t, res.Segments)
		for _, seg := range res.Segments {
			assert.LessOrEqual(t, len(seg), segmentBytes)
		}
		assert.Equal(t, pattern(MaxResponseBytes), concat(res))
	})
	t.Run("one byte over completes as too large", func(t *testing.T) {
		s := New()
		f := arm(s, "req-1")
		rec := postTo(s, "response", "req-1", &patterned{n: MaxResponseBytes + 1}, -1)
		assert.Equal(t, http.StatusRequestEntityTooLarge, rec.Code)
		res := (await(t, f.done)).res
		assert.Equal(t, ErrTypeResponseTooLarge, errType(t, res.Payload))
	})
	t.Run("empty body is an empty non-nil payload", func(t *testing.T) {
		s := New()
		f := arm(s, "req-1")
		postTo(s, "response", "req-1", &patterned{}, -1)
		res := (await(t, f.done)).res
		require.NotNil(t, res.Segments)
		assert.Empty(t, concat(res))
	})
	t.Run("peak allocation stays within the cap plus one segment", func(t *testing.T) {
		for _, n := range []int{MaxResponseBytes, MaxResponseBytes + 1} {
			br := testing.Benchmark(func(b *testing.B) {
				b.ReportAllocs()
				for range b.N {
					s := New()
					f := arm(s, "req-1")
					postTo(s, "response", "req-1", &patterned{n: n}, -1)
					await(t, f.done)
				}
			})
			assert.Less(t, br.AllocedBytesPerOp(), int64(MaxResponseBytes+segmentBytes+64<<10), "body of %d bytes", n)
		}
	})
}

func TestErrorPostOverCapKeepsTruncatedPayload(t *testing.T) {
	s := New()
	f := arm(s, "req-1")
	n := MaxResponseBytes + 100
	rec := postTo(s, "error", "req-1", &patterned{n: n}, int64(n))
	assert.Equal(t, http.StatusAccepted, rec.Code)
	res := (await(t, f.done)).res
	assert.Equal(t, ErrTypeUnhandled, res.FunctionError)
	assert.Equal(t, pattern(MaxResponseBytes), res.Payload)
}

func TestBootstrapExitBeforeAdmissionCancels(t *testing.T) {
	t.Run("before the flight exists", func(t *testing.T) {
		s := New()
		assert.True(t, s.BootstrapExited())
		ctx := t.Context()
		go func() { _, _ = s.Invoke(ctx, inv("req-1", time.Minute)) }()
		require.Eventually(t, func() bool {
			s.mu.Lock()
			defer s.mu.Unlock()
			return s.cur != nil && s.cur.state == stExited
		}, time.Second, time.Millisecond, "the flight is created already exited")
	})
	t.Run("before next", func(t *testing.T) {
		s := New()
		f := arm(s, "req-1")
		f.state = stWaiting
		assert.True(t, s.BootstrapExited())
		assert.Equal(t, stExited, f.state)
	})
	t.Run("after next", func(t *testing.T) {
		s := New()
		f := arm(s, "req-1")
		assert.True(t, s.BootstrapExited())
		assert.Equal(t, stExited, f.state)
	})
}

func TestExitWinsTheRace(t *testing.T) {
	s := New()
	f := arm(s, "req-1")
	require.True(t, s.BootstrapExited())

	c := &counting{}
	rec := postTo(s, "response", "req-1", c, 4)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Equal(t, "InvalidRequestID", errType(t, rec.Body.Bytes()))
	assert.Zero(t, c.reads.Load())
	assert.Empty(t, f.done)
}

func TestAdmissionWinsTheRace(t *testing.T) {
	s := New()
	f := arm(s, "req-1")
	adm := s.admit("req-1")
	require.NotNil(t, adm)
	assert.False(t, s.BootstrapExited(), "an admitted response is not cancelled by the exit")

	r := httptest.NewRequest(http.MethodPost, "/", bytes.NewReader([]byte("late body")))
	rec := httptest.NewRecorder()
	s.finishResponse(rec, r, adm)
	assert.Equal(t, http.StatusAccepted, rec.Code)
	assert.Equal(t, "late body", string((await(t, f.done)).res.Payload))
	assert.False(t, s.BootstrapExited(), "a recorded result stands")
}

func TestExitedAfterResultIsIgnored(t *testing.T) {
	s := New()
	f := arm(s, "req-1")
	postTo(s, "response", "req-1", bytes.NewReader([]byte("ok")), 2)
	await(t, f.done)
	assert.False(t, s.BootstrapExited())
}

// failing yields some bytes and then a read error, as a reset connection does.
type failing struct{ left int }

func (f *failing) Read(b []byte) (int, error) {
	if f.left == 0 {
		return 0, io.ErrClosedPipe
	}
	n := min(len(b), f.left)
	f.left -= n
	return n, nil
}

// invokeReady runs Invoke on a real flight that the runtime has taken.
func invokeReady(t *testing.T, s *Server, ctx context.Context) <-chan invokeOut {
	t.Helper()
	out := make(chan invokeOut, 1)
	go func() {
		res, err := s.Invoke(ctx, inv("req-1", time.Minute))
		out <- invokeOut{res, err}
	}()
	require.Eventually(t, func() bool {
		s.mu.Lock()
		defer s.mu.Unlock()
		if s.cur != nil && s.cur.state == stWaiting {
			s.cur.state = stRunning
		}
		return s.cur != nil && s.cur.state == stRunning
	}, time.Second, time.Millisecond)
	return out
}

func TestLiveBootstrapTruncatedBodyEndsInvokeAtOnce(t *testing.T) {
	defer goleak.VerifyNone(t, goleak.IgnoreCurrent())
	s := New()
	out := invokeReady(t, s, context.Background()) // a minute-long deadline
	adm := s.admit("req-1")
	require.NotNil(t, adm)
	s.finishResponse(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/", &failing{left: 3}), adm)
	select {
	case got := <-out:
		require.ErrorIs(t, got.err, ErrResponseBodyFailed)
		assert.Empty(t, got.res.Payload)
		assert.Empty(t, got.res.FunctionError)
	case <-time.After(5 * time.Second):
		t.Fatal("Invoke waited for its deadline")
	}
}

func TestIncompleteBodyNeverStrandsInvoke(t *testing.T) {
	// A body that fails after admission loses the result. The flight must end
	// with an explicit outcome whichever of the read failure, the bootstrap
	// exit and the invocation ending comes first.
	steps := map[string]func(s *Server, adm *flight, cancel context.CancelFunc){
		"read fails": func(s *Server, adm *flight, _ context.CancelFunc) {
			s.finishResponse(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/", &failing{left: 3}), adm)
		},
		"bootstrap exits":      func(s *Server, _ *flight, _ context.CancelFunc) { s.BootstrapExited() },
		"invocation cancelled": func(_ *Server, _ *flight, cancel context.CancelFunc) { cancel() },
	}
	orders := [][]string{
		{"read fails", "bootstrap exits", "invocation cancelled"},
		{"bootstrap exits", "read fails", "invocation cancelled"},
		{"invocation cancelled", "read fails", "bootstrap exits"},
		{"invocation cancelled", "bootstrap exits", "read fails"},
		{"read fails", "invocation cancelled", "bootstrap exits"},
		{"bootstrap exits", "invocation cancelled", "read fails"},
	}
	for _, order := range orders {
		t.Run(order[0]+"_"+order[1]+"_"+order[2], func(t *testing.T) {
			defer goleak.VerifyNone(t, goleak.IgnoreCurrent())
			s := New()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			out := invokeReady(t, s, ctx)
			adm := s.admit("req-1")
			require.NotNil(t, adm)
			// Whichever of the read failure and the cancellation comes first
			// decides the outcome; the exit changes nothing once admitted.
			want := ErrResponseBodyFailed
			for _, step := range order {
				if step == "invocation cancelled" {
					want = context.Canceled
				}
				if step != "bootstrap exits" {
					break
				}
			}
			var got invokeOut
			returned := false
			for _, step := range order {
				steps[step](s, adm, cancel)
				if step == "invocation cancelled" {
					// Settle the cancellation before the next transition.
					select {
					case got = <-out:
						returned = true
					case <-time.After(5 * time.Second):
						t.Fatal("Invoke did not return after cancel")
					}
				}
			}
			if !returned {
				select {
				case got = <-out:
				case <-time.After(5 * time.Second):
					t.Fatal("Invoke did not return")
				}
			}
			require.ErrorIs(t, got.err, want)
			assert.Empty(t, got.res.Payload)
			assert.Empty(t, got.res.FunctionError)
			s.mu.Lock()
			defer s.mu.Unlock()
			assert.Nil(t, s.cur, "the flight must be detached")
		})
	}
}

func TestIncompleteBodyAndDeadlineInBothOrders(t *testing.T) {
	t.Run("deadline first", func(t *testing.T) {
		defer goleak.VerifyNone(t, goleak.IgnoreCurrent())
		s := New()
		out := make(chan invokeOut, 1)
		go func() {
			res, err := s.Invoke(context.Background(), inv("req-1", 20*time.Millisecond))
			out <- invokeOut{res, err}
		}()
		require.Eventually(t, func() bool {
			s.mu.Lock()
			defer s.mu.Unlock()
			if s.cur != nil && s.cur.state == stWaiting {
				s.cur.state = stRunning
			}
			return s.cur != nil
		}, time.Second, time.Millisecond)
		adm := s.admit("req-1")
		require.NotNil(t, adm)
		select {
		case got := <-out:
			require.ErrorIs(t, got.err, ErrDeadlineExceeded)
		case <-time.After(5 * time.Second):
			t.Fatal("Invoke did not time out")
		}
		s.finishResponse(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/", &failing{left: 1}), adm)
		assert.Empty(t, adm.done, "a late failure publishes nothing")
	})
	t.Run("body failure first", func(t *testing.T) {
		defer goleak.VerifyNone(t, goleak.IgnoreCurrent())
		s := New()
		out := invokeReady(t, s, context.Background())
		adm := s.admit("req-1")
		require.NotNil(t, adm)
		s.finishResponse(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/", &failing{left: 1}), adm)
		require.ErrorIs(t, (await(t, out)).err, ErrResponseBodyFailed)
	})
}

// The reply a bootstrap sees to an oversize body is recorded here, not
// claimed to match AWS.
func TestOversizeBootstrapVisibleReply(t *testing.T) {
	cases := map[string]struct {
		length int64
		size   int
	}{
		"declared length": {MaxResponseBytes + 2, MaxResponseBytes + 2},
		"chunked":         {-1, MaxResponseBytes + 2},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			s, ts := start(t)
			out := invokeAsync(s, inv("req-1", 10*time.Second))
			get(t, ts.URL+base+"invocation/next")
			req, err := http.NewRequest(http.MethodPost, ts.URL+base+"invocation/req-1/response", io.NopCloser(&patterned{n: tc.size}))
			require.NoError(t, err)
			req.ContentLength = tc.length
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Logf("bootstrap-visible: transport error: %v", err)
			} else {
				defer resp.Body.Close()
				t.Logf("bootstrap-visible: status %d", resp.StatusCode)
				assert.Equal(t, http.StatusRequestEntityTooLarge, resp.StatusCode)
			}
			got := await(t, out)
			require.NoError(t, got.err)
			assert.Equal(t, ErrTypeResponseTooLarge, errType(t, got.res.Payload))
		})
	}
}
