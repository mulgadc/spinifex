package runtimeapi

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const base = "/2018-06-01/runtime/"

type invokeOut struct {
	res Result
	err error
}

func start(t *testing.T) (*Server, *httptest.Server) {
	t.Helper()
	s := New()
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	return s, ts
}

func invokeAsync(s *Server, inv Invocation) <-chan invokeOut {
	ch := make(chan invokeOut, 1)
	go func() {
		res, err := s.Invoke(context.Background(), inv)
		ch <- invokeOut{res, err}
	}()
	return ch
}

func post(t *testing.T, url string, body []byte, hdr map[string]string) (*http.Response, []byte) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(body))
	require.NoError(t, err)
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp, b
}

func get(t *testing.T, url string) (*http.Response, []byte) {
	t.Helper()
	resp, err := http.Get(url)
	require.NoError(t, err)
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp, b
}

func inv(id string, d time.Duration) Invocation {
	return Invocation{
		RequestID:   id,
		FunctionARN: "arn:aws:lambda:ap-southeast-2:000000000001:function:f",
		Deadline:    time.Now().Add(d),
		Payload:     []byte(`{"a":1}`),
	}
}

func errType(t *testing.T, body []byte) string {
	t.Helper()
	var e map[string]string
	require.NoError(t, json.Unmarshal(body, &e))
	assert.NotEmpty(t, e["errorMessage"])
	return e["errorType"]
}

func TestNextResponseRoundTrip(t *testing.T) {
	s, ts := start(t)
	in := inv("req-1", 2*time.Second)
	in.TraceID = "Root=1-abc"
	out := invokeAsync(s, in)

	resp, body := get(t, ts.URL+base+"invocation/next")
	require.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Equal(t, `{"a":1}`, string(body))
	assert.Equal(t, "req-1", resp.Header.Get("Lambda-Runtime-Aws-Request-Id"))
	assert.Equal(t, strconv.FormatInt(in.Deadline.UnixMilli(), 10), resp.Header.Get("Lambda-Runtime-Deadline-Ms"))
	assert.Equal(t, in.FunctionARN, resp.Header.Get("Lambda-Runtime-Invoked-Function-Arn"))
	assert.Equal(t, "Root=1-abc", resp.Header.Get("Lambda-Runtime-Trace-Id"))

	r, _ := post(t, ts.URL+base+"invocation/req-1/response", []byte(`{"sum":3}`), nil)
	assert.Equal(t, http.StatusAccepted, r.StatusCode)

	got := <-out
	require.NoError(t, got.err)
	assert.Equal(t, `{"sum":3}`, string(got.res.Payload))
	assert.Empty(t, got.res.FunctionError)
}

func TestTraceHeaderOmittedWhenUnset(t *testing.T) {
	s, ts := start(t)
	invokeAsync(s, inv("req-1", time.Second))
	resp, _ := get(t, ts.URL+base+"invocation/next")
	_, present := resp.Header["Lambda-Runtime-Trace-Id"]
	assert.False(t, present)
}

func TestErrorPostKeepsBodyAndType(t *testing.T) {
	s, ts := start(t)
	out := invokeAsync(s, inv("req-1", 2*time.Second))
	get(t, ts.URL+base+"invocation/next")

	body := []byte(`{"errorMessage":"boom","errorType":"Custom"}`)
	r, _ := post(t, ts.URL+base+"invocation/req-1/error", body, map[string]string{"Lambda-Runtime-Function-Error-Type": "Custom"})
	assert.Equal(t, http.StatusAccepted, r.StatusCode)

	got := <-out
	require.NoError(t, got.err)
	assert.Equal(t, "Custom", got.res.FunctionError)
	assert.JSONEq(t, string(body), string(got.res.Payload))
}

func TestErrorPostWithoutTypeIsUnhandled(t *testing.T) {
	s, ts := start(t)
	out := invokeAsync(s, inv("req-1", 2*time.Second))
	get(t, ts.URL+base+"invocation/next")
	post(t, ts.URL+base+"invocation/req-1/error", []byte(`{}`), nil)
	assert.Equal(t, "Unhandled", (<-out).res.FunctionError)
}

func TestUnknownAndRepeatedIDRefused(t *testing.T) {
	s, ts := start(t)

	r, b := post(t, ts.URL+base+"invocation/nope/response", []byte("x"), nil)
	assert.Equal(t, http.StatusBadRequest, r.StatusCode)
	assert.Equal(t, "InvalidRequestID", errType(t, b))

	out := invokeAsync(s, inv("req-1", 2*time.Second))
	get(t, ts.URL+base+"invocation/next")
	post(t, ts.URL+base+"invocation/req-1/response", []byte("ok"), nil)
	<-out

	r, b = post(t, ts.URL+base+"invocation/req-1/response", []byte("again"), nil)
	assert.Equal(t, http.StatusBadRequest, r.StatusCode)
	assert.Equal(t, "InvalidRequestID", errType(t, b))
	r, _ = post(t, ts.URL+base+"invocation/req-1/error", []byte("{}"), nil)
	assert.Equal(t, http.StatusBadRequest, r.StatusCode)
}

func TestResponseNotAcceptedBeforeDelivery(t *testing.T) {
	s, ts := start(t)
	invokeAsync(s, inv("req-1", time.Second))
	r, _ := post(t, ts.URL+base+"invocation/req-1/response", []byte("early"), nil)
	assert.Equal(t, http.StatusBadRequest, r.StatusCode)
}

func TestOversizeResponse(t *testing.T) {
	s, ts := start(t)
	out := invokeAsync(s, inv("req-1", 5*time.Second))
	get(t, ts.URL+base+"invocation/next")

	r, b := post(t, ts.URL+base+"invocation/req-1/response", make([]byte, MaxResponseBytes+1), nil)
	assert.Equal(t, http.StatusRequestEntityTooLarge, r.StatusCode)
	assert.Equal(t, "RequestEntityTooLarge", errType(t, b))

	got := <-out
	require.NoError(t, got.err)
	assert.Equal(t, "Unhandled", got.res.FunctionError)
	assert.Equal(t, "Function.ResponseSizeTooLarge", errType(t, got.res.Payload))
}

func TestResponseAtLimitAccepted(t *testing.T) {
	s, ts := start(t)
	out := invokeAsync(s, inv("req-1", 5*time.Second))
	get(t, ts.URL+base+"invocation/next")
	r, _ := post(t, ts.URL+base+"invocation/req-1/response", make([]byte, MaxResponseBytes), nil)
	assert.Equal(t, http.StatusAccepted, r.StatusCode)
	got := <-out
	require.NoError(t, got.err)
	assert.Len(t, got.res.Payload, MaxResponseBytes)
}

func TestDeadlineExpiryRefusesLateResponse(t *testing.T) {
	s, ts := start(t)
	out := invokeAsync(s, inv("req-1", 40*time.Millisecond))
	get(t, ts.URL+base+"invocation/next")

	got := <-out
	require.ErrorIs(t, got.err, ErrDeadlineExceeded)

	r, b := post(t, ts.URL+base+"invocation/req-1/response", []byte("late"), nil)
	assert.Equal(t, http.StatusBadRequest, r.StatusCode)
	assert.Equal(t, "InvalidRequestID", errType(t, b))

	// The slot is free again for the next invocation.
	out = invokeAsync(s, inv("req-2", 2*time.Second))
	resp, _ := get(t, ts.URL+base+"invocation/next")
	assert.Equal(t, "req-2", resp.Header.Get("Lambda-Runtime-Aws-Request-Id"))
	post(t, ts.URL+base+"invocation/req-2/response", []byte("ok"), nil)
	require.NoError(t, (<-out).err)
}

func TestConcurrentInvokeRefused(t *testing.T) {
	s, ts := start(t)
	out := invokeAsync(s, inv("req-1", 2*time.Second))
	require.Eventually(t, func() bool {
		s.mu.Lock()
		defer s.mu.Unlock()
		return s.cur != nil
	}, time.Second, time.Millisecond)

	_, err := s.Invoke(context.Background(), inv("req-2", time.Second))
	require.ErrorIs(t, err, ErrBusy)

	get(t, ts.URL+base+"invocation/next")
	post(t, ts.URL+base+"invocation/req-1/response", []byte("ok"), nil)
	require.NoError(t, (<-out).err)
}

func TestInvokeContextCancel(t *testing.T) {
	s, _ := start(t)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := s.Invoke(ctx, inv("req-1", time.Minute))
		done <- err
	}()
	cancel()
	require.ErrorIs(t, <-done, context.Canceled)
}

func TestNextBlocksUntilInvoke(t *testing.T) {
	s, ts := start(t)
	type got struct {
		resp *http.Response
		body []byte
	}
	ch := make(chan got, 1)
	go func() {
		resp, err := http.Get(ts.URL + base + "invocation/next") //nolint:noctx // test client
		if err != nil {
			ch <- got{}
			return
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		ch <- got{resp, body}
	}()

	select {
	case <-ch:
		t.Fatal("next returned before any invocation")
	case <-time.After(50 * time.Millisecond):
	}

	out := invokeAsync(s, inv("req-1", 2*time.Second))
	g := <-ch
	require.NotNil(t, g.resp, "next request failed")
	assert.Equal(t, "req-1", g.resp.Header.Get("Lambda-Runtime-Aws-Request-Id"))
	post(t, ts.URL+base+"invocation/req-1/response", []byte("ok"), nil)
	require.NoError(t, (<-out).err)
}

func TestInitErrorRecorded(t *testing.T) {
	s, ts := start(t)
	_, ok := s.InitError()
	require.False(t, ok)

	r, _ := post(t, ts.URL+base+"init/error", []byte(`{"errorMessage":"bad"}`), map[string]string{"Lambda-Runtime-Function-Error-Type": "Runtime.InitError"})
	assert.Equal(t, http.StatusAccepted, r.StatusCode)

	select {
	case <-s.InitFailed():
	default:
		t.Fatal("InitFailed not closed")
	}
	ie, ok := s.InitError()
	require.True(t, ok)
	assert.Equal(t, "Runtime.InitError", ie.Type)
	assert.JSONEq(t, `{"errorMessage":"bad"}`, string(ie.Body))
}

func TestInvokeRequiresRequestID(t *testing.T) {
	s := New()
	_, err := s.Invoke(context.Background(), Invocation{Deadline: time.Now().Add(time.Second)})
	require.ErrorIs(t, err, ErrInvalidInvocation)
}
