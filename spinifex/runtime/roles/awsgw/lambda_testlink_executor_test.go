//go:build lambdatestlink

package awsgw

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	testutil "github.com/mulgadc/spinifex/internal/testkit"
	"github.com/mulgadc/spinifex/spinifex/domains/lambda/awsapi"
	"github.com/mulgadc/spinifex/spinifex/domains/lambda/codestore"
	"github.com/mulgadc/spinifex/spinifex/domains/lambda/execution"
	"github.com/mulgadc/spinifex/spinifex/domains/lambda/function"
	"github.com/mulgadc/spinifex/spinifex/runtime/compute/lambda/testlink"
	"github.com/nats-io/nats.go"
	"github.com/stretchr/testify/require"
)

const secretURL = "https://store.example/code?X-Amz-Signature=secret-signature"

type fakePresigner struct{ err error }

func (f fakePresigner) PresignGet(context.Context, codestore.Ref, string, time.Duration) (string, error) {
	return secretURL, f.err
}

type fakeNATS struct {
	max   int64
	sent  [][]byte
	reply *testlink.Reply
	err   error
}

func (f *fakeNATS) MaxPayload() int64 { return f.max }

func (f *fakeNATS) RequestWithContext(_ context.Context, _ string, data []byte) (*nats.Msg, error) {
	f.sent = append(f.sent, data)
	if f.err != nil {
		return nil, f.err
	}
	body, _ := json.Marshal(f.reply)
	return &nats.Msg{Data: body}, nil
}

func invocation(payload []byte) awsapi.SyncInvocation {
	return awsapi.SyncInvocation{Payload: payload, Function: function.Record{
		FunctionID: "fn1", AccountID: "123456789012", Region: "ap-southeast-2", FunctionName: "f",
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

func executor(n *fakeNATS, presignErr error, store *execution.Store) *testLinkExecutor {
	return &testLinkExecutor{NATS: n, Node: "node1",
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
			n, store := &fakeNATS{max: tc.max}, grantStore(t)
			_, err := executor(n, tc.presignErr, store).InvokeSync(context.Background(), tc.in)
			var refusal *awsapi.RefusalError
			require.ErrorAs(t, err, &refusal)
			require.Empty(t, n.sent, "a refusal must send nothing")
			require.NotContains(t, err.Error(), "secret-signature")
			require.Equal(t, tc.state, grantState(t, store), "no grant, or one withdrawn unsent")
		})
	}
}

func TestLambdaTestLinkExecutor_BusyNodeSendsNothing(t *testing.T) {
	store := grantStore(t)
	held := &fakeNATS{max: 1 << 20, err: nats.ErrTimeout}
	_, err := executor(held, nil, store).InvokeSync(context.Background(), invocation(nil))
	require.Error(t, err)

	n := &fakeNATS{max: 1 << 20, reply: &testlink.Reply{Outcome: testlink.OutcomeDone}}
	_, err = executor(n, nil, store).InvokeSync(context.Background(), invocation(nil))
	var refusal *awsapi.RefusalError
	require.ErrorAs(t, err, &refusal)
	require.ErrorIs(t, err, execution.ErrNodeBusy)
	require.Empty(t, n.sent, "no second grant while an attempt of unknown outcome holds the node")
}

// 3000 payload bytes are 4000 as base64: the bound counts the encoding.
func TestLambdaTestLinkExecutor_BoundCountsEncodedPayload(t *testing.T) {
	n := &fakeNATS{max: 3500, reply: &testlink.Reply{Outcome: testlink.OutcomeDone}}
	_, err := executor(n, nil, grantStore(t)).InvokeSync(context.Background(), invocation(make([]byte, 3000)))
	var refusal *awsapi.RefusalError
	require.ErrorAs(t, err, &refusal)
	require.Empty(t, n.sent)
}

func TestLambdaTestLinkExecutor_PostDispatchFailureIsNeverARefusal(t *testing.T) {
	cases := map[string]*fakeNATS{
		"no reply":      {max: 1 << 20, err: nats.ErrTimeout},
		"no responders": {max: 1 << 20, err: nats.ErrNoResponders},
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
	n := &fakeNATS{max: 1 << 20, reply: &testlink.Reply{Outcome: testlink.OutcomeRefused, Error: "busy"}}
	_, err := executor(n, nil, store).InvokeSync(context.Background(), invocation(nil))
	var refusal *awsapi.RefusalError
	require.ErrorAs(t, err, &refusal)
	require.Equal(t, execution.StateWithdrawn, grantState(t, store), "an unstarted grant the daemon refused is withdrawn")

	n = &fakeNATS{max: 1 << 20, reply: &testlink.Reply{Outcome: testlink.OutcomeDone, Payload: []byte(`{"sum":42}`), FunctionError: "Unhandled"}}
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
