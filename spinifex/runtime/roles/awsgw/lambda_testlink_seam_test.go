//go:build lambdatestlink && lambdatestlinkseam

package awsgw

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/mulgadc/spinifex/spinifex/agents/lambda/agent"
	"github.com/mulgadc/spinifex/spinifex/agents/lambda/hostlink"
	"github.com/mulgadc/spinifex/spinifex/agents/lambda/runtimeapi"
	lambdaawsapi "github.com/mulgadc/spinifex/spinifex/domains/lambda/awsapi"
	"github.com/mulgadc/spinifex/spinifex/domains/lambda/codestore"
	"github.com/mulgadc/spinifex/spinifex/domains/lambda/function"
	"github.com/mulgadc/spinifex/spinifex/runtime/compute/lambda/testlink"
	"github.com/nats-io/nats.go"
	"github.com/stretchr/testify/require"
)

// seamCredential stands in for a presigned credential; it is not a real key.
const seamCredential = "AKIASYNTHETICSEAMKEY"

var seamCode = []byte("synthetic function code")

// seamFunction serves one Active function "f" whose stored code is seamCode.
type seamFunction struct{ timeout int64 }

var _ function.Client = seamFunction{}

func (s seamFunction) Get(_ context.Context, account, region, name string) (function.Record, uint64, error) {
	if name != "f" {
		return function.Record{}, 0, function.ErrNotFound
	}
	sum := sha256.Sum256(seamCode)
	return function.Record{FunctionID: "fn1", AccountID: account, Region: region, FunctionName: name,
		Runtime: "provided.al2023", Timeout: s.timeout, MemorySize: 128, State: function.StateActive,
		Code: function.Code{RevisionID: "r1", State: function.CodeStored, SHA256: hex.EncodeToString(sum[:]), Size: int64(len(seamCode))},
	}, 1, nil
}

func (seamFunction) Create(context.Context, string, string, function.Record) (uint64, error) {
	return 0, errors.New("unused")
}

func (seamFunction) CodeStored(context.Context, string, string, string, uint64, string, int64) (function.Record, uint64, error) {
	return function.Record{}, 0, errors.New("unused")
}

func (seamFunction) RollbackCreate(context.Context, string, string, string, uint64) error {
	return errors.New("unused")
}

type urlPresigner struct{ url string }

func (p urlPresigner) PresignGet(context.Context, codestore.Ref, string, time.Duration) (string, error) {
	return p.url, nil
}

// seamNATS is the transport only: it hands the executor's encoded request to
// the daemon's real attempt steps and returns the daemon's encoded reply.
type seamNATS struct {
	t       *testing.T
	d       *testlink.Daemon
	guest   func(context.Context, net.Conn)
	mu      sync.Mutex
	replies [][]byte
}

func (*seamNATS) MaxPayload() int64 { return 1 << 20 }

func (n *seamNATS) RequestWithContext(ctx context.Context, subj string, data []byte) (*nats.Msg, error) {
	require.Equal(n.t, testlink.Subject("node1"), subj)
	body, err := json.Marshal(n.d.HandleWithGuest(ctx, data, n.guest))
	n.mu.Lock()
	n.replies = append(n.replies, body)
	n.mu.Unlock()
	return &nats.Msg{Data: body}, err
}

// skewAhead moves every deadline the agent sees earlier, as a guest clock
// running that far ahead of the host's would.
type skewAhead struct {
	hostlink.Link
	ahead time.Duration
}

func (l skewAhead) Next(ctx context.Context) (hostlink.Request, error) {
	r, err := l.Link.Next(ctx)
	r.DeadlineMs -= l.ahead.Milliseconds()
	return r, err
}

// agentGuest runs the real guest agent and Runtime API server; bootstrap is
// an HTTP client standing in for the function runtime.
func agentGuest(ahead time.Duration, bootstrap func(base string)) func(context.Context, net.Conn) {
	return func(ctx context.Context, conn net.Conn) {
		srv := runtimeapi.New()
		ts := httptest.NewServer(srv.Handler())
		defer ts.Close()
		if bootstrap != nil {
			go bootstrap(ts.URL + "/2018-06-01/runtime/")
		}
		var link hostlink.Link = hostlink.NewTestStream(conn)
		if ahead > 0 {
			link = skewAhead{link, ahead}
		}
		_ = agent.Run(ctx, link, srv)
	}
}

func nextInvocationID(base string) (string, bool) {
	resp, err := http.Get(base + "invocation/next")
	if err != nil {
		return "", false
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	return resp.Header.Get("Lambda-Runtime-Aws-Request-Id"), true
}

// lockedBuffer collects log output written from several goroutines.
type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

type seamRun struct {
	resp    *http.Response
	body    []byte
	replies [][]byte
	log     string
}

// invokeComposed sends one signed Invoke through the composed gateway, the
// Lambda handler and the real test-link executor into the daemon's attempt.
func invokeComposed(t *testing.T, timeout int64, codeURL string, client *http.Client, guest func(context.Context, net.Conn)) seamRun {
	t.Helper()
	logs := &lockedBuffer{}
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	store := grantStore(t)
	n := &seamNATS{t: t, d: &testlink.Daemon{Node: "node1", Grants: store, HTTP: client}, guest: guest}
	exec := &testLinkExecutor{NATS: n, Node: "node1",
		Codes:  func(context.Context) (codePresigner, error) { return urlPresigner{codeURL}, nil },
		Grants: func(context.Context) (grantIssuer, error) { return store, nil },
	}
	reg := lambdaawsapi.NewRegistration(lambdaawsapi.Deps{Meta: seamFunction{timeout: timeout}, Exec: exec})
	resp := signedLambdaInvoke(t, defaultGateway(t, lambdaTestRoutes(reg)...))
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	n.mu.Lock()
	defer n.mu.Unlock()
	require.Len(t, n.replies, 1, "the request reached the daemon once")
	return seamRun{resp: resp, body: body, replies: n.replies, log: logs.String()}
}

func codeServer(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(seamCode)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func daemonReply(t *testing.T, raw []byte) testlink.Reply {
	t.Helper()
	var rep testlink.Reply
	require.NoError(t, json.Unmarshal(raw, &rep))
	return rep
}

// An oversized function response reaches the caller as 200 with an Unhandled
// function error and the runtime's error payload.
func TestLambdaTestLinkSeam_OversizedResponseIsUnhandled(t *testing.T) {
	srv := codeServer(t)
	run := invokeComposed(t, 10, srv.URL+"/code", srv.Client(), agentGuest(0, func(base string) {
		if id, ok := nextInvocationID(base); ok {
			if resp, err := http.Post(base+"invocation/"+id+"/response", "application/json",
				bytes.NewReader(make([]byte, runtimeapi.MaxResponseBytes+1))); err == nil {
				_ = resp.Body.Close()
			}
		}
	}))
	require.Equal(t, testlink.OutcomeDone, daemonReply(t, run.replies[0]).Outcome)
	require.Equal(t, http.StatusOK, run.resp.StatusCode)
	require.Equal(t, "Unhandled", run.resp.Header.Get("X-Amz-Function-Error"))
	var payload struct {
		ErrorType string `json:"errorType"`
	}
	require.NoError(t, json.Unmarshal(run.body, &payload))
	require.Equal(t, "Function.ResponseSizeTooLarge", payload.ErrorType)
}

// Neither an early guest timeout claim nor the host's deadline is a function
// result: both reach the caller as a 500 ServiceException.
func TestLambdaTestLinkSeam_TimeoutsAreUnknown(t *testing.T) {
	cases := map[string]struct {
		timeout int64
		guest   func(context.Context, net.Conn)
	}{
		"agent timeout before the host deadline": {2, agentGuest(2*time.Second-200*time.Millisecond, func(base string) { nextInvocationID(base) })},
		"host deadline":                          {1, func(ctx context.Context, _ net.Conn) { <-ctx.Done() }},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			srv := codeServer(t)
			run := invokeComposed(t, tc.timeout, srv.URL+"/code", srv.Client(), tc.guest)
			require.Equal(t, testlink.OutcomeUnknown, daemonReply(t, run.replies[0]).Outcome)
			require.Equal(t, http.StatusInternalServerError, run.resp.StatusCode)
			require.Equal(t, "ServiceException", run.resp.Header.Get("X-Amzn-Errortype"))
			require.Empty(t, run.resp.Header.Get("X-Amz-Function-Error"))
			require.NotContains(t, string(run.body), "Sandbox.Timedout")
		})
	}
}

// A malformed redirect quoting a presigned credential reaches neither the
// daemon's reply, the caller, nor the gateway's log.
func TestLambdaTestLinkSeam_RedirectCredentialNotLeaked(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Location", "%zz?X-Amz-Credential="+seamCredential+"&X-Amz-Signature=deadbeef")
		w.WriteHeader(http.StatusFound)
	}))
	t.Cleanup(srv.Close)
	run := invokeComposed(t, 3, srv.URL+"/code", srv.Client(), func(ctx context.Context, _ net.Conn) { <-ctx.Done() })

	rep := daemonReply(t, run.replies[0])
	require.Equal(t, testlink.OutcomeRefused, rep.Outcome)
	require.Equal(t, "code download: request failed", rep.Error)
	require.Contains(t, run.log, "code download: request failed", "the refusal was not logged")
	require.NotContains(t, string(run.replies[0]), seamCredential)
	require.NotContains(t, string(run.body), seamCredential)
	require.NotContains(t, run.log, seamCredential)
	require.Equal(t, http.StatusInternalServerError, run.resp.StatusCode)
}
