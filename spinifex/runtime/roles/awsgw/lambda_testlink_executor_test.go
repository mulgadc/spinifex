//go:build lambdatestlink

package awsgw

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/sts"
	testutil "github.com/mulgadc/spinifex/internal/testkit"
	"github.com/mulgadc/spinifex/spinifex/domains/lambda/awsapi"
	"github.com/mulgadc/spinifex/spinifex/domains/lambda/codestore"
	"github.com/mulgadc/spinifex/spinifex/domains/lambda/execution"
	"github.com/mulgadc/spinifex/spinifex/domains/lambda/function"
	stsdomain "github.com/mulgadc/spinifex/spinifex/domains/sts"
	"github.com/mulgadc/spinifex/spinifex/runtime/compute/lambda/testlink"
	"github.com/stretchr/testify/require"
)

const secretURL = "https://store.example/code?X-Amz-Signature=secret-signature"

type fakePresigner struct{ err error }

func (f fakePresigner) PresignGet(context.Context, codestore.Ref, string, time.Duration) (string, error) {
	return secretURL, f.err
}

// fakeControl stands in for the daemon's socket. An ErrNotSent error is
// returned before recording, as the real client does.
type fakeControl struct {
	max     int64
	sent    [][]byte
	creds   []testlink.Credential
	replyBy time.Time
	reply   *testlink.Reply
	err     error
	// during runs inside the exchange, after the request is recorded.
	during func()
}

func (f *fakeControl) Exchange(ctx context.Context, req []byte, cred testlink.Credential, replyBy time.Time) (testlink.Reply, error) {
	if errors.Is(f.err, testlink.ErrNotSent) {
		return testlink.Reply{}, f.err
	}
	f.sent = append(f.sent, req)
	f.creds = append(f.creds, cred)
	f.replyBy = replyBy
	if f.during != nil {
		f.during()
	}
	if f.err != nil {
		return testlink.Reply{}, f.err
	}
	return *f.reply, nil
}

const testSecret = "fake-secret-access-key"

// fakeSessions issues one fixed session and records revokes.
type fakeSessions struct {
	issued    []stsdomain.LambdaEnvironmentSessionRequest
	revoked   []stsdomain.LambdaEnvironmentSessionScope
	revokeCtx error
	issueErr  error
	revokeErr error
}

func (f *fakeSessions) AssumeRoleForLambdaEnvironment(_ context.Context, req stsdomain.LambdaEnvironmentSessionRequest) (*sts.AssumeRoleOutput, error) {
	f.issued = append(f.issued, req)
	if f.issueErr != nil {
		return nil, f.issueErr
	}
	return &sts.AssumeRoleOutput{Credentials: &sts.Credentials{
		AccessKeyId: aws.String("ASIAFAKEKEY"), SecretAccessKey: aws.String(testSecret), SessionToken: aws.String("fake-token"),
	}}, nil
}

func (f *fakeSessions) RevokeLambdaEnvironmentSession(ctx context.Context, accessKeyID string, scope stsdomain.LambdaEnvironmentSessionScope) error {
	if accessKeyID != "ASIAFAKEKEY" {
		return errors.New("revoked an unknown key")
	}
	f.revokeCtx = ctx.Err()
	f.revoked = append(f.revoked, scope)
	return f.revokeErr
}

func invocation(payload []byte) awsapi.SyncInvocation {
	return awsapi.SyncInvocation{Payload: payload, Function: function.Record{
		FunctionID: "fn1", AccountID: "123456789012", Region: "ap-southeast-2", FunctionName: "f",
		Role:    "arn:aws:iam::123456789012:role/fn-exec",
		Handler: "bootstrap.h", Timeout: 3, MemorySize: 128,
		Code: function.Code{RevisionID: "r1", SHA256: strings.Repeat("ab", 32), Size: 100},
	}}
}

func grantStore(t *testing.T) *execution.Store {
	t.Helper()
	_, nc, _ := testutil.StartTestJetStream(t)
	store, err := execution.Open(context.Background(), testutil.NewJetStream(t, nc))
	require.NoError(t, err)
	return store
}

func executor(n *fakeControl, presignErr error, store *execution.Store) *testLinkExecutor {
	return executorWith(n, &fakeSessions{}, presignErr, store)
}

// testImageDigest stands in for the build's pinned runtime image digest.
const testImageDigest = "ab12ab12ab12ab12ab12ab12ab12ab12ab12ab12ab12ab12ab12ab12ab12ab12"

func executorWith(n *fakeControl, sessions *fakeSessions, presignErr error, store *execution.Store) *testLinkExecutor {
	return &testLinkExecutor{Control: n, RequestLimit: n.max, Sessions: sessions, Node: "node1", ImageDigest: testImageDigest,
		Codes:  func(context.Context) (codePresigner, error) { return fakePresigner{err: presignErr}, nil },
		Grants: func(context.Context) (grantIssuer, error) { return store, nil },
	}
}

// grantState is the node's current attempt state, or "" with no grant.
func grantState(t *testing.T, store *execution.Store) string {
	t.Helper()
	a, err := store.Current(context.Background(), "node1")
	if errors.Is(err, execution.ErrNotCurrent) {
		return ""
	}
	require.NoError(t, err)
	return a.State
}

func TestLambdaTestLinkExecutor_RefusesWithoutSending(t *testing.T) {
	cases := map[string]struct {
		in         awsapi.SyncInvocation
		presignErr error
		max        int64
		state      string
	}{
		"no code digest":    {in: func() awsapi.SyncInvocation { i := invocation(nil); i.Function.Code.SHA256 = ""; return i }(), max: 1 << 20},
		"presign fails":     {in: invocation(nil), presignErr: errors.New("boom"), max: 1 << 20},
		"encoded too large": {in: invocation(make([]byte, 3000)), max: 4000, state: execution.StateWithdrawn},
		"code beyond bound": {in: func() awsapi.SyncInvocation {
			i := invocation(nil)
			i.Function.Code.Size = testlink.MaxCodeBytes + 1
			return i
		}(), max: 1 << 20},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			n, store := &fakeControl{max: tc.max}, grantStore(t)
			_, err := executor(n, tc.presignErr, store).InvokeSync(context.Background(), tc.in)
			var refusal *awsapi.RefusalError
			require.ErrorAs(t, err, &refusal)
			require.Empty(t, n.sent, "a refusal must send nothing")
			require.NotContains(t, err.Error(), "secret-signature")
			require.Equal(t, tc.state, grantState(t, store), "no grant, or one withdrawn unsent")
		})
	}
}

// A gateway built without a valid image pin refuses before presigning,
// granting, issuing or connecting.
func TestLambdaTestLinkExecutor_InvalidImagePinRefusesBeforeConnecting(t *testing.T) {
	for name, pin := range map[string]string{"missing": "", "malformed": "not-a-digest"} {
		t.Run(name, func(t *testing.T) {
			n, store := &fakeControl{max: 1 << 20, reply: &testlink.Reply{Outcome: testlink.OutcomeDone}}, grantStore(t)
			sessions := &fakeSessions{}
			e := executorWith(n, sessions, nil, store)
			e.ImageDigest = pin
			_, err := e.InvokeSync(context.Background(), invocation(nil))
			var refusal *awsapi.RefusalError
			require.ErrorAs(t, err, &refusal)
			require.Empty(t, n.sent)
			require.Empty(t, sessions.issued)
			require.Empty(t, grantState(t, store), "no grant taken")
		})
	}
}

func TestLambdaTestLinkExecutor_BusyNodeSendsNothing(t *testing.T) {
	store := grantStore(t)
	held := &fakeControl{max: 1 << 20, err: errors.New("no reply")}
	_, err := executor(held, nil, store).InvokeSync(context.Background(), invocation(nil))
	require.Error(t, err)

	n := &fakeControl{max: 1 << 20, reply: &testlink.Reply{Outcome: testlink.OutcomeDone}}
	_, err = executor(n, nil, store).InvokeSync(context.Background(), invocation(nil))
	var refusal *awsapi.RefusalError
	require.ErrorAs(t, err, &refusal)
	require.ErrorIs(t, err, execution.ErrNodeBusy)
	require.Empty(t, n.sent, "no second grant while an attempt of unknown outcome holds the node")
}

// 3000 payload bytes are 4000 as base64: the bound counts the encoding.
func TestLambdaTestLinkExecutor_BoundCountsEncodedPayload(t *testing.T) {
	n := &fakeControl{max: 3500, reply: &testlink.Reply{Outcome: testlink.OutcomeDone}}
	_, err := executor(n, nil, grantStore(t)).InvokeSync(context.Background(), invocation(make([]byte, 3000)))
	var refusal *awsapi.RefusalError
	require.ErrorAs(t, err, &refusal)
	require.Empty(t, n.sent)
}

func TestLambdaTestLinkExecutor_PostDispatchFailureIsNeverARefusal(t *testing.T) {
	cases := map[string]*fakeControl{
		"no reply":      {max: 1 << 20, err: errors.New("read test-link reply: i/o timeout")},
		"partial write": {max: 1 << 20, err: errors.New("send test-link request: broken pipe")},
		"unknown":       {max: 1 << 20, reply: &testlink.Reply{Outcome: testlink.OutcomeUnknown, Error: "VM stop not proven"}},
		"unrecognised":  {max: 1 << 20, reply: &testlink.Reply{Outcome: "later"}},
	}
	for name, n := range cases {
		t.Run(name, func(t *testing.T) {
			store := grantStore(t)
			_, err := executor(n, nil, store).InvokeSync(context.Background(), invocation([]byte(`{}`)))
			require.Error(t, err)
			var refusal *awsapi.RefusalError
			require.False(t, errors.As(err, &refusal), "a sent request has an unknown outcome")
			require.Len(t, n.sent, 1)
			require.NotContains(t, err.Error(), "secret-signature")
			require.Equal(t, execution.StateGranted, grantState(t, store), "a sent grant is never withdrawn")
		})
	}
}

func TestLambdaTestLinkExecutor_DaemonRefusalAndResult(t *testing.T) {
	store := grantStore(t)
	n := &fakeControl{max: 1 << 20, reply: &testlink.Reply{Outcome: testlink.OutcomeRefused, Error: "busy"}}
	_, err := executor(n, nil, store).InvokeSync(context.Background(), invocation(nil))
	var refusal *awsapi.RefusalError
	require.ErrorAs(t, err, &refusal)
	require.Equal(t, execution.StateWithdrawn, grantState(t, store), "an unstarted grant the daemon refused is withdrawn")

	n = &fakeControl{max: 1 << 20, reply: &testlink.Reply{Outcome: testlink.OutcomeDone, Payload: []byte(`{"sum":42}`), FunctionError: "Unhandled"}}
	out, err := executor(n, nil, store).InvokeSync(context.Background(), invocation([]byte(`{"a":1}`)))
	require.NoError(t, err)
	require.Equal(t, awsapi.SyncResult{Payload: []byte(`{"sum":42}`), FunctionError: "Unhandled", ExecutedVersion: "$LATEST"}, out)

	var sent testlink.Request
	require.NoError(t, json.Unmarshal(n.sent[0], &sent))
	require.Equal(t, secretURL, sent.CodeURL)
	require.Equal(t, int64(3000), sent.TimeoutMs)
	require.Equal(t, "bootstrap.h", sent.Handler)
	require.Regexp(t, `^[0-9a-f]{12}$`, sent.AttemptID)
	require.NoError(t, testlink.CheckRequest(sent))

	// The request carries exactly the grant and identity the owner recorded.
	cur, err := store.Current(context.Background(), "node1")
	require.NoError(t, err)
	require.Equal(t, cur.Grant, sent.Grant)
	require.Equal(t, cur.Identity, sent.Identity())
	require.Equal(t, "r1", cur.Identity.RevisionID)
}

var testScope = stsdomain.LambdaEnvironmentSessionScope{
	AccountID: "123456789012", FunctionID: "fn1",
	FunctionARN: "arn:aws:lambda:ap-southeast-2:123456789012:function:f",
}

func TestLambdaTestLinkExecutor_IssuerFailureRefusesUnsent(t *testing.T) {
	store := grantStore(t)
	n := &fakeControl{max: 1 << 20, reply: &testlink.Reply{Outcome: testlink.OutcomeDone}}
	sessions := &fakeSessions{issueErr: errors.New("AccessDenied")}
	_, err := executorWith(n, sessions, nil, store).InvokeSync(context.Background(), invocation(nil))
	var refusal *awsapi.RefusalError
	require.ErrorAs(t, err, &refusal)
	require.Empty(t, n.sent, "nothing is sent without a session")
	require.Empty(t, sessions.revoked)
	require.Equal(t, execution.StateWithdrawn, grantState(t, store))
}

// The session is issued for the attempt's own environment under the base ARN,
// with the attempt's whole lifetime, and goes only on the socket.
func TestLambdaTestLinkExecutor_IssuesForTheAttemptAndSendsOnTheSocket(t *testing.T) {
	store := grantStore(t)
	n := &fakeControl{max: 1 << 20, reply: &testlink.Reply{Outcome: testlink.OutcomeDone}}
	sessions := &fakeSessions{}
	in := invocation(nil)
	in.Qualifier = "$LATEST"
	start := time.Now()
	_, err := executorWith(n, sessions, nil, store).InvokeSync(context.Background(), in)
	require.NoError(t, err)

	require.Len(t, sessions.issued, 1)
	got := sessions.issued[0]
	cur, err := store.Current(context.Background(), "node1")
	require.NoError(t, err)
	require.Equal(t, cur.Grant.EnvironmentID, got.EnvironmentID)
	require.Equal(t, "arn:aws:iam::123456789012:role/fn-exec", got.RoleARN)
	require.Equal(t, testScope.FunctionARN, got.FunctionARN, "STS sees the base ARN, never the qualified one")
	lifetime := testlink.AttemptLifetime(3000)
	require.Equal(t, int64(lifetime/time.Second), got.MaxLifetimeSeconds)
	require.WithinDuration(t, start.Add(lifetime+replyGrace), n.replyBy, 5*time.Second)

	var sent testlink.Request
	require.NoError(t, json.Unmarshal(n.sent[0], &sent))
	require.Equal(t, testScope.FunctionARN+":$LATEST", sent.FunctionARN, "the runtime still sees the invoked ARN")
	require.Equal(t, testImageDigest, sent.ImageDigest, "the daemon checks the gateway's pinned image")
	require.NotContains(t, string(n.sent[0]), testSecret, "the credential travels beside the request, not in it")
	require.Equal(t, testSecret, n.creds[0].SecretAccessKey)

	want := testScope
	want.EnvironmentID = cur.Grant.EnvironmentID
	require.Equal(t, []stsdomain.LambdaEnvironmentSessionScope{want}, sessions.revoked)
}

func TestLambdaTestLinkExecutor_RevokesOnEveryOutcome(t *testing.T) {
	cases := map[string]struct {
		n       *fakeControl
		refusal bool
	}{
		"done":          {n: &fakeControl{reply: &testlink.Reply{Outcome: testlink.OutcomeDone}}},
		"refused":       {n: &fakeControl{reply: &testlink.Reply{Outcome: testlink.OutcomeRefused}}, refusal: true},
		"unknown":       {n: &fakeControl{reply: &testlink.Reply{Outcome: testlink.OutcomeUnknown}}},
		"lost":          {n: &fakeControl{err: errors.New("read test-link reply: EOF")}},
		"partial write": {n: &fakeControl{err: errors.New("send test-link request: broken pipe")}},
		"not sent":      {n: &fakeControl{err: fmt.Errorf("%w: dial", testlink.ErrNotSent)}, refusal: true},
		"wrong peer":    {n: &fakeControl{err: fmt.Errorf("%w: %w", testlink.ErrNotSent, testlink.ErrPeer)}, refusal: true},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			store := grantStore(t)
			tc.n.max = 1 << 20
			sessions := &fakeSessions{}
			_, err := executorWith(tc.n, sessions, nil, store).InvokeSync(context.Background(), invocation(nil))
			var refusal *awsapi.RefusalError
			require.Equal(t, tc.refusal, errors.As(err, &refusal), "err: %v", err)
			require.Len(t, sessions.revoked, 1, "revoked exactly once")
			if tc.refusal {
				require.Equal(t, execution.StateWithdrawn, grantState(t, store))
			}
		})
	}
}

// A cancelled Invoke still revokes, under a context of its own.
func TestLambdaTestLinkExecutor_CancelledInvokeStillRevokes(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	n := &fakeControl{max: 1 << 20, err: errors.New("read test-link reply: canceled"), during: cancel}
	sessions := &fakeSessions{}
	_, err := executorWith(n, sessions, nil, grantStore(t)).InvokeSync(ctx, invocation(nil))
	require.Error(t, err)
	require.Len(t, sessions.revoked, 1)
	require.NoError(t, sessions.revokeCtx, "the revoke ran under a live context")
}

// A revoke that fails after the write makes the outcome unknown, whether the
// daemon answered done or an authenticated refused.
func TestLambdaTestLinkExecutor_FailedRevokeIsNeverSuccessOrRefusal(t *testing.T) {
	for name, rep := range map[string]testlink.Reply{
		"done":    {Outcome: testlink.OutcomeDone, Payload: []byte("ok")},
		"refused": {Outcome: testlink.OutcomeRefused, Error: "busy"},
	} {
		t.Run(name, func(t *testing.T) {
			n := &fakeControl{max: 1 << 20, reply: &rep}
			sessions := &fakeSessions{revokeErr: stsdomain.ErrLambdaSessionRevokeFailed}
			out, err := executorWith(n, sessions, nil, grantStore(t)).InvokeSync(context.Background(), invocation(nil))
			require.ErrorIs(t, err, stsdomain.ErrLambdaSessionRevokeFailed)
			var refusal *awsapi.RefusalError
			require.False(t, errors.As(err, &refusal))
			require.Empty(t, out.Payload)
			require.NotContains(t, err.Error(), "ASIAFAKEKEY")
			require.NotContains(t, err.Error(), testSecret)
		})
	}
}

func TestLambdaTestLinkExecutor_OversizeRequestRevokesUnsent(t *testing.T) {
	store := grantStore(t)
	n := &fakeControl{max: 4000, reply: &testlink.Reply{Outcome: testlink.OutcomeDone}}
	sessions := &fakeSessions{}
	_, err := executorWith(n, sessions, nil, store).InvokeSync(context.Background(), invocation(make([]byte, 3000)))
	var refusal *awsapi.RefusalError
	require.ErrorAs(t, err, &refusal)
	require.Empty(t, n.sent)
	require.Len(t, sessions.revoked, 1)
	require.Equal(t, execution.StateWithdrawn, grantState(t, store))
}

// With the real client, a daemon that reads the request and never answers is
// unknown at the reply bound, and the session is revoked.
func TestLambdaTestLinkExecutor_SilentDaemonIsUnknownAndRevoked(t *testing.T) {
	name := fmt.Sprintf("spinifex-test-exec-%d", time.Now().UnixNano())
	ln, err := net.ListenUnix("unix", &net.UnixAddr{Name: "@" + name, Net: "unix"})
	require.NoError(t, err)
	hold, read := make(chan struct{}), make(chan error, 1)
	t.Cleanup(func() { close(hold); _ = ln.Close() })
	go func() {
		c, err := ln.Accept()
		if err != nil {
			read <- err
			return
		}
		defer func() { _ = c.Close() }()
		_, err = io.Copy(io.Discard, io.LimitReader(c, 64))
		read <- err
		<-hold
	}()

	e := executorWith(&fakeControl{max: 1 << 20}, &fakeSessions{}, nil, grantStore(t))
	sessions := e.Sessions.(*fakeSessions)
	e.Control = &testlink.ControlClient{Name: name, DaemonUID: os.Getuid(), RequestLimit: 1 << 20, ReplyLimit: 1 << 20, IOTimeout: time.Hour}
	// Shift the clock so the reply bound falls 300 ms from now.
	shift := testlink.AttemptLifetime(3000) + replyGrace - 300*time.Millisecond
	e.now = func() time.Time { return time.Now().Add(-shift) }

	start := time.Now()
	_, err = e.InvokeSync(context.Background(), invocation(nil))
	require.Error(t, err)
	select {
	case rerr := <-read:
		require.NoError(t, rerr)
	case <-time.After(10 * time.Second):
		t.Fatal("daemon read nothing")
	}
	var refusal *awsapi.RefusalError
	require.False(t, errors.As(err, &refusal), "a delivered request is unknown")
	require.Less(t, time.Since(start), 10*time.Second)
	require.Len(t, sessions.revoked, 1)
}

// withdrawFault fails Withdraw before or after the owner writes it.
type withdrawFault struct {
	*execution.Store
	afterWrite bool
}

func (w withdrawFault) Withdraw(ctx context.Context, g execution.Grant) error {
	if !w.afterWrite {
		return errors.New("owner unavailable")
	}
	if err := w.Store.Withdraw(ctx, g); err != nil {
		return err
	}
	return execution.ErrOutcomeUnknown
}

// A daemon refusal with a failed Withdraw leaves the grant startable: a
// replayed Start succeeds. A lost Withdraw acknowledgement leaves it fenced.
func TestLambdaTestLinkExecutor_RefusalWithdrawFaults(t *testing.T) {
	for name, tc := range map[string]struct {
		afterWrite bool
		state      string
		replay     bool
	}{
		"fails before write": {state: execution.StateGranted, replay: true},
		"ack lost":           {afterWrite: true, state: execution.StateWithdrawn},
	} {
		t.Run(name, func(t *testing.T) {
			store := grantStore(t)
			n := &fakeControl{max: 1 << 20, reply: &testlink.Reply{Outcome: testlink.OutcomeRefused, Error: "image differs"}}
			e := executor(n, nil, store)
			e.Grants = func(context.Context) (grantIssuer, error) { return withdrawFault{store, tc.afterWrite}, nil }
			_, err := e.InvokeSync(context.Background(), invocation(nil))
			require.Error(t, err)
			require.Len(t, n.sent, 1)
			require.Equal(t, tc.state, grantState(t, store))

			var sent testlink.Request
			require.NoError(t, json.Unmarshal(n.sent[0], &sent))
			err = store.Start(context.Background(), sent.Grant, sent.Identity())
			if tc.replay {
				require.NoError(t, err, "an unwithdrawn grant can still start")
			} else {
				require.Error(t, err, "a withdrawn grant cannot start")
			}
		})
	}
}

// The digest field counts toward the request bound: the exact encoded size
// is sent and one byte less is refused unsent.
func TestLambdaTestLinkExecutor_RequestBoundIncludesImageDigest(t *testing.T) {
	probe := &fakeControl{max: 1 << 20, reply: &testlink.Reply{Outcome: testlink.OutcomeDone}}
	_, err := executor(probe, nil, grantStore(t)).InvokeSync(context.Background(), invocation(make([]byte, 1000)))
	require.NoError(t, err)
	require.Len(t, probe.sent, 1)
	require.Contains(t, string(probe.sent[0]), `"image_digest":"`+testImageDigest+`"`)
	size := int64(len(probe.sent[0]))

	at := &fakeControl{max: size, reply: &testlink.Reply{Outcome: testlink.OutcomeDone}}
	_, err = executor(at, nil, grantStore(t)).InvokeSync(context.Background(), invocation(make([]byte, 1000)))
	require.NoError(t, err)
	require.Len(t, at.sent, 1)

	under := &fakeControl{max: size - 1}
	_, err = executor(under, nil, grantStore(t)).InvokeSync(context.Background(), invocation(make([]byte, 1000)))
	var refusal *awsapi.RefusalError
	require.ErrorAs(t, err, &refusal)
	require.Empty(t, under.sent)
}
