package agent

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mulgadc/spinifex/spinifex/agents/lambda/hostlink"
	"github.com/mulgadc/spinifex/spinifex/agents/lambda/runtimeapi"
)

const api = "/2018-06-01/runtime/"

// fakeLink feeds scripted requests and records completions.
type fakeLink struct {
	reqs []hostlink.Request
	got  chan hostlink.Response
}

var _ hostlink.Link = (*fakeLink)(nil)

func (l *fakeLink) Next(ctx context.Context) (hostlink.Request, error) {
	if len(l.reqs) == 0 {
		return hostlink.Request{}, hostlink.ErrShutdown
	}
	r := l.reqs[0]
	l.reqs = l.reqs[1:]
	return r, nil
}

func (l *fakeLink) Complete(ctx context.Context, r hostlink.Response) error {
	l.got <- r
	return nil
}

// runtime polls next and answers using reply.
func runtime(t *testing.T, base string, reply func(id string, body []byte) (path string, out string)) {
	t.Helper()
	go func() {
		for {
			resp, err := http.Get(base + api + "invocation/next")
			if err != nil {
				return
			}
			body, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			id := resp.Header.Get("Lambda-Runtime-Aws-Request-Id")
			path, out := reply(id, body)
			r, err := http.Post(base+api+"invocation/"+id+path, "application/json", strings.NewReader(out))
			if err != nil {
				return
			}
			r.Body.Close()
		}
	}()
}

func deadline(d time.Duration) int64 { return time.Now().Add(d).UnixMilli() }

func TestRunServesRequestsAndEchoesIDs(t *testing.T) {
	srv := runtimeapi.New()
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	runtime(t, ts.URL, func(id string, body []byte) (string, string) {
		if string(body) == "bad" {
			return "/error", `{"errorMessage":"no"}`
		}
		return "/response", "echo:" + string(body)
	})

	link := &fakeLink{
		got: make(chan hostlink.Response, 4),
		reqs: []hostlink.Request{
			{GrantID: "g1", AttemptID: "a1", RequestID: "r1", FunctionARN: "arn", DeadlineMs: deadline(2 * time.Second), Payload: []byte("one")},
			{GrantID: "g2", AttemptID: "a2", RequestID: "r2", FunctionARN: "arn", DeadlineMs: deadline(2 * time.Second), Payload: []byte("bad")},
		},
	}
	require.NoError(t, Run(context.Background(), link, srv))

	r1, r2 := <-link.got, <-link.got
	assert.Equal(t, hostlink.Response{GrantID: "g1", AttemptID: "a1", RequestID: "r1", Payload: []byte("echo:one")}, r1)
	assert.Equal(t, "g2", r2.GrantID)
	assert.Equal(t, "a2", r2.AttemptID)
	assert.Equal(t, "Unhandled", r2.FunctionError)
}

func TestRunReportsTimeoutAndStops(t *testing.T) {
	srv := runtimeapi.New() // no runtime polling: the invocation expires
	link := &fakeLink{
		got: make(chan hostlink.Response, 2),
		reqs: []hostlink.Request{
			{GrantID: "g", AttemptID: "a", RequestID: "r", DeadlineMs: deadline(30 * time.Millisecond)},
			{GrantID: "g2", AttemptID: "a2", RequestID: "r2", DeadlineMs: deadline(2 * time.Second)},
		},
	}
	require.ErrorIs(t, Run(context.Background(), link, srv), ErrInvocationTimedOut)
	r := <-link.got
	assert.Equal(t, "Sandbox.Timedout", r.FunctionError)
	assert.Equal(t, "g", r.GrantID)
	assert.Len(t, link.reqs, 1, "the next request must not be taken")
	assert.Empty(t, link.got, "nothing else is reported")
}

// blockLink blocks in Next until ctx ends.
type blockLink struct{ fakeLink }

func (blockLink) Next(ctx context.Context) (hostlink.Request, error) {
	<-ctx.Done()
	return hostlink.Request{}, ctx.Err()
}

func TestRunStopsOnInitError(t *testing.T) {
	srv := runtimeapi.New()
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	r, err := http.Post(ts.URL+api+"init/error", "application/json", strings.NewReader(`{"errorMessage":"x"}`))
	require.NoError(t, err)
	r.Body.Close()

	err = Run(context.Background(), &blockLink{}, srv)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "init error")
}

type errLink struct{ fakeLink }

func (errLink) Next(context.Context) (hostlink.Request, error) {
	return hostlink.Request{}, errors.New("link down")
}

func TestRunReturnsLinkError(t *testing.T) {
	err := Run(context.Background(), &errLink{}, runtimeapi.New())
	require.EqualError(t, err, "link down")
}
