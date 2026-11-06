//go:build lambdatestlink

// In package awsgw: the poller, its session handling and its in-process
// transport are unexported test-link internals.
package awsgw

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/credentials"
	"github.com/aws/aws-sdk-go/aws/request"
	"github.com/aws/aws-sdk-go/service/sqs"
	"github.com/aws/aws-sdk-go/service/sqs/sqsiface"
	"github.com/aws/aws-sdk-go/service/sts"
	testutil "github.com/mulgadc/spinifex/internal/testkit"
	"github.com/mulgadc/spinifex/spinifex/domains/lambda/awsapi"
	"github.com/mulgadc/spinifex/spinifex/domains/lambda/eventsource"
	"github.com/mulgadc/spinifex/spinifex/domains/lambda/function"
	stsdomain "github.com/mulgadc/spinifex/spinifex/domains/sts"
	"github.com/stretchr/testify/require"
)

const esmQueueARN = "arn:aws:sqs:ap-southeast-2:123456789012:in"

// esmQueue is an SQS fake: Receive hands out queued messages once, Delete
// records what was deleted.
type esmQueue struct {
	sqsiface.SQSAPI
	mu         sync.Mutex
	pending    []*sqs.Message
	deleted    []string
	receives   int
	receiveErr error
	urls       []string
}

func (q *esmQueue) ReceiveMessageWithContext(ctx aws.Context, in *sqs.ReceiveMessageInput, _ ...request.Option) (*sqs.ReceiveMessageOutput, error) {
	q.mu.Lock()
	q.receives++
	q.urls = append(q.urls, aws.StringValue(in.QueueUrl))
	err := q.receiveErr
	n := min(int(aws.Int64Value(in.MaxNumberOfMessages)), len(q.pending))
	msgs := q.pending[:n]
	q.pending = q.pending[n:]
	q.mu.Unlock()
	if err != nil {
		return nil, err
	}
	if len(msgs) == 0 {
		t := time.NewTimer(20 * time.Millisecond)
		defer t.Stop()
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-t.C:
		}
	}
	return &sqs.ReceiveMessageOutput{Messages: msgs}, nil
}

func (q *esmQueue) DeleteMessageBatchWithContext(_ aws.Context, in *sqs.DeleteMessageBatchInput, _ ...request.Option) (*sqs.DeleteMessageBatchOutput, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	for _, e := range in.Entries {
		q.deleted = append(q.deleted, aws.StringValue(e.ReceiptHandle))
	}
	return &sqs.DeleteMessageBatchOutput{}, nil
}

func (q *esmQueue) state() (deleted []string, receives int) {
	q.mu.Lock()
	defer q.mu.Unlock()
	return append([]string(nil), q.deleted...), q.receives
}

// esmInvokes records each event and returns the configured outcome.
type esmInvokes struct {
	mu       sync.Mutex
	payloads []string
	result   awsapi.SyncResult
	err      error
	// started, when set, is closed as the first call begins, which then
	// blocks until release is closed.
	started, release chan struct{}
}

func (f *esmInvokes) InvokeEventSource(_ context.Context, _, _, _ string, payload []byte) (awsapi.SyncResult, error) {
	f.mu.Lock()
	f.payloads = append(f.payloads, string(payload))
	started, release := f.started, f.release
	f.started = nil
	f.mu.Unlock()
	if started != nil {
		close(started)
		<-release
	}
	return f.result, f.err
}

func (f *esmInvokes) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.payloads)
}

// esmSessions issues numbered sessions with a fixed lifetime.
type esmSessions struct {
	mu       sync.Mutex
	life     time.Duration
	issued   int
	revoked  []string
	issueErr error
}

func (f *esmSessions) AssumeRoleForLambdaEnvironment(_ context.Context, _ stsdomain.LambdaEnvironmentSessionRequest) (*sts.AssumeRoleOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.issueErr != nil {
		return nil, f.issueErr
	}
	f.issued++
	id := "ASIA" + strings.Repeat("K", f.issued)
	return &sts.AssumeRoleOutput{Credentials: &sts.Credentials{
		AccessKeyId: aws.String(id), SecretAccessKey: aws.String("s"), SessionToken: aws.String("t"),
		Expiration: aws.Time(time.Now().Add(f.life)),
	}}, nil
}

func (f *esmSessions) RevokeLambdaEnvironmentSession(_ context.Context, keyID string, _ stsdomain.LambdaEnvironmentSessionScope) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.revoked = append(f.revoked, keyID)
	return nil
}

type esmMeta struct{ function.Client }

func (esmMeta) Get(_ context.Context, accountID, region, name string) (function.Record, uint64, error) {
	return function.Record{FunctionID: "fn1", AccountID: accountID, Region: region, FunctionName: name,
		Role: "arn:aws:iam::123456789012:role/fn-exec", Timeout: 3}, 1, nil
}

func esmMapping(id string) eventsource.Mapping {
	return eventsource.Mapping{
		FormatVersion: eventsource.FormatVersion, UUID: id, AccountID: "123456789012", Region: "ap-southeast-2",
		FunctionName: "f", EventSourceARN: esmQueueARN, BatchSize: 10, State: eventsource.StateEnabled,
		StateTransitionReason: eventsource.ReasonUserInitiated,
	}
}

func msg(id, body string) *sqs.Message {
	return &sqs.Message{MessageId: aws.String(id), ReceiptHandle: aws.String("rh-" + id), Body: aws.String(body)}
}

func newTestESM(t *testing.T, q *esmQueue, inv *esmInvokes) (*esmRuntime, *eventsource.Store, *esmSessions) {
	t.Helper()
	_, _, js := testutil.StartTestJetStream(t)
	store, err := eventsource.Open(context.Background(), js)
	require.NoError(t, err)
	sessions := &esmSessions{life: time.Hour}
	r := newESMRuntime(func(context.Context) (*eventsource.Store, error) { return store, nil }, sessions, "ap-southeast-2")
	r.meta = esmMeta{}
	r.invoke = inv
	r.queue = func(*credentials.Credentials) sqsiface.SQSAPI { return q }
	r.endpoint = "https://gw.example:9999"
	r.interval = 20 * time.Millisecond
	r.backoff = 10 * time.Millisecond
	return r, store, sessions
}

// Verifies: ADR-0009:S16
func TestESM_DeletesOnlyOnClassifiedSuccess(t *testing.T) {
	cases := []struct {
		name   string
		result awsapi.SyncResult
		err    error
		delete bool
	}{
		{name: "success", result: awsapi.SyncResult{ExecutedVersion: "$LATEST", Payload: []byte(`{}`)}, delete: true},
		{name: "function error", result: awsapi.SyncResult{ExecutedVersion: "$LATEST", FunctionError: "Unhandled"}},
		{name: "refusal", err: &awsapi.RefusalError{Err: errors.New("node busy")}},
		{name: "unknown outcome", err: errors.New("the invocation outcome is unknown")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			q := &esmQueue{}
			inv := &esmInvokes{result: tc.result, err: tc.err}
			r, _, _ := newTestESM(t, q, inv)
			r.process(context.Background(), esmMapping("u1"), q, "url", []*sqs.Message{msg("1", "a"), msg("2", "b")})
			require.Equal(t, 1, inv.count())
			deleted, _ := q.state()
			if tc.delete {
				require.Equal(t, []string{"rh-1", "rh-2"}, deleted)
			} else {
				require.Empty(t, deleted)
			}
		})
	}
}

func TestESM_EventShapeAndSizeSplit(t *testing.T) {
	m := esmMapping("u1")
	quoted := strings.Repeat(`"`, 700*1024) // each byte encodes as two
	msgs := []*sqs.Message{}
	for i := range 6 {
		msgs = append(msgs, msg(string(rune('a'+i)), quoted))
	}
	events, oversized := splitEvents(m, msgs)
	require.Empty(t, oversized)
	require.Len(t, events, 2, "8.4 MiB of encoded records must split under the 6 MiB bound")
	total := 0
	for _, ev := range events {
		require.LessOrEqual(t, len(ev.Payload), esmMaxEvent)
		total += len(ev.Messages)
	}
	require.Equal(t, 6, total)
	require.Contains(t, string(events[0].Payload), `"eventSource":"aws:sqs"`)
	require.Contains(t, string(events[0].Payload), `"eventSourceARN":"`+esmQueueARN+`"`)
	require.Contains(t, string(events[0].Payload), `"messageAttributes":{}`)

	huge := strings.Repeat("\x00", esmMaxEvent/6+1) // each byte encodes as \u0000
	events, oversized = splitEvents(m, []*sqs.Message{msg("big", huge), msg("ok", "small")})
	require.Len(t, oversized, 1)
	require.Equal(t, "big", aws.StringValue(oversized[0].MessageId))
	require.Len(t, events, 1)

	q := &esmQueue{}
	inv := &esmInvokes{result: awsapi.SyncResult{ExecutedVersion: "$LATEST"}}
	r, _, _ := newTestESM(t, q, inv)
	r.process(context.Background(), m, q, "url", []*sqs.Message{msg("big", huge)})
	require.Zero(t, inv.count(), "an oversized message is never invoked")
	deleted, _ := q.state()
	require.Empty(t, deleted, "an oversized message is never deleted")
}

// A mapping written before the gateway starts is polled, and a Deleting
// record is removed only after its worker stopped; nothing follows it.
func TestESM_RestartReloadAndDeleteStopsWorker(t *testing.T) {
	q := &esmQueue{pending: []*sqs.Message{msg("1", "hello")}}
	inv := &esmInvokes{result: awsapi.SyncResult{ExecutedVersion: "$LATEST"}}
	r, store, _ := newTestESM(t, q, inv)
	ctx := t.Context()
	require.NoError(t, store.Create(ctx, esmMapping("u1")))

	go r.run(ctx)
	require.Eventually(t, func() bool { d, _ := q.state(); return len(d) == 1 }, 5*time.Second, 10*time.Millisecond)
	_, receives := q.state()
	require.Positive(t, receives)
	q.mu.Lock()
	firstURL := q.urls[0]
	q.mu.Unlock()
	require.Equal(t, "https://gw.example:9999/123456789012/in", firstURL)

	m, rev, err := store.Get(ctx, "u1")
	require.NoError(t, err)
	m.State = eventsource.StateDeleting
	_, err = store.Update(ctx, m, rev)
	require.NoError(t, err)
	require.Eventually(t, func() bool {
		_, _, err := store.Get(ctx, "u1")
		return errors.Is(err, eventsource.ErrNotFound)
	}, 5*time.Second, 10*time.Millisecond)

	_, after := q.state()
	q.mu.Lock()
	q.pending = []*sqs.Message{msg("2", "late")}
	q.mu.Unlock()
	time.Sleep(200 * time.Millisecond)
	deleted, receivesNow := q.state()
	require.Equal(t, after, receivesNow, "no receive after the delete completed")
	require.Equal(t, []string{"rh-1"}, deleted)
	require.Equal(t, 1, inv.count())
}

// A delete requested while a batch is running completes only after that batch
// finished, so its message delete precedes the record's removal.
func TestESM_DeleteWaitsForInFlightBatch(t *testing.T) {
	q := &esmQueue{pending: []*sqs.Message{msg("1", "hello")}}
	inv := &esmInvokes{result: awsapi.SyncResult{ExecutedVersion: "$LATEST"},
		started: make(chan struct{}), release: make(chan struct{})}
	started := inv.started
	r, store, _ := newTestESM(t, q, inv)
	ctx := t.Context()
	require.NoError(t, store.Create(ctx, esmMapping("u4")))
	go r.run(ctx)
	<-started

	m, rev, err := store.Get(ctx, "u4")
	require.NoError(t, err)
	m.State = eventsource.StateDeleting
	_, err = store.Update(ctx, m, rev)
	require.NoError(t, err)
	require.Never(t, func() bool {
		_, _, err := store.Get(ctx, "u4")
		return errors.Is(err, eventsource.ErrNotFound)
	}, 300*time.Millisecond, 10*time.Millisecond, "the delete must wait for the running batch")

	close(inv.release)
	require.Eventually(t, func() bool {
		_, _, err := store.Get(ctx, "u4")
		return errors.Is(err, eventsource.ErrNotFound)
	}, 5*time.Second, 10*time.Millisecond)
	deleted, _ := q.state()
	require.Equal(t, []string{"rh-1"}, deleted, "the finished batch's message was deleted before removal")
}

// A Deleting record left by a crash before the gateway restarted is removed.
func TestESM_InterruptedDeleteResumes(t *testing.T) {
	q := &esmQueue{}
	r, store, _ := newTestESM(t, q, &esmInvokes{})
	ctx := t.Context()
	m := esmMapping("u2")
	m.State = eventsource.StateDeleting
	require.NoError(t, store.Create(ctx, m))
	go r.run(ctx)
	require.Eventually(t, func() bool {
		_, _, err := store.Get(ctx, "u2")
		return errors.Is(err, eventsource.ErrNotFound)
	}, 5*time.Second, 10*time.Millisecond)
	_, receives := q.state()
	require.Zero(t, receives)
}

func TestESM_SessionReissuedBeforeExpiry(t *testing.T) {
	r, _, sessions := newTestESM(t, &esmQueue{}, &esmInvokes{})
	rec, _, _ := esmMeta{}.Get(context.Background(), "123456789012", "ap-southeast-2", "f")
	sessions.life = esmRenewBefore + time.Minute
	s, err := r.fresh(context.Background(), rec, nil)
	require.NoError(t, err)
	same, err := r.fresh(context.Background(), rec, s)
	require.NoError(t, err)
	require.Same(t, s, same, "a session far from expiry is kept")

	r.now = func() time.Time { return time.Now().Add(2 * time.Minute) }
	next, err := r.fresh(context.Background(), rec, s)
	require.NoError(t, err)
	require.NotSame(t, s, next)
	require.Equal(t, []string{s.keyID}, sessions.revoked, "the replaced session is revoked")

	rec.Role = "arn:aws:iam::123456789012:role/other"
	r.now = time.Now
	third, err := r.fresh(context.Background(), rec, next)
	require.NoError(t, err)
	require.NotSame(t, next, third, "a changed execution role gets a new session")
}

func TestESM_ReceiveFailureRecordedAndCleared(t *testing.T) {
	q := &esmQueue{receiveErr: errors.New("AWS.SimpleQueueService.NonExistentQueue: The specified queue does not exist.")}
	r, store, _ := newTestESM(t, q, &esmInvokes{})
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	require.NoError(t, store.Create(ctx, esmMapping("u3")))
	done := make(chan struct{})
	go func() { defer close(done); r.work(ctx, esmMapping("u3")) }()
	require.Eventually(t, func() bool {
		m, _, err := store.Get(ctx, "u3")
		return err == nil && strings.HasPrefix(m.StateTransitionReason, "PROBLEM: ") &&
			strings.Contains(m.StateTransitionReason, "NonExistentQueue")
	}, 5*time.Second, 10*time.Millisecond)
	m, _, err := store.Get(ctx, "u3")
	require.NoError(t, err)
	require.Equal(t, eventsource.StateEnabled, m.State, "a receive failure keeps the mapping")

	q.mu.Lock()
	q.receiveErr = nil
	q.mu.Unlock()
	require.Eventually(t, func() bool {
		m, _, err := store.Get(ctx, "u3")
		return err == nil && m.StateTransitionReason == eventsource.ReasonUserInitiated
	}, 5*time.Second, 10*time.Millisecond)
	cancel()
	<-done
}

func TestESM_HandlerTransportCarriesSignedRequest(t *testing.T) {
	var got *http.Request
	var body string
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r
		b, _ := io.ReadAll(r.Body)
		body = string(b)
		w.Header().Set("Content-Type", "application/x-amz-json-1.0")
		_, _ = w.Write([]byte(`{"Attributes":{"VisibilityTimeout":"45"}}`))
	})
	r, _, _ := newTestESM(t, &esmQueue{}, &esmInvokes{})
	r.handler = h
	q := r.handlerQueue(credentials.NewStaticCredentials("ASIAX", "secret", "token"))
	out, err := q.GetQueueAttributes(&sqs.GetQueueAttributesInput{QueueUrl: aws.String("https://gw.example:9999/123456789012/in"),
		AttributeNames: aws.StringSlice([]string{"VisibilityTimeout"})})
	require.NoError(t, err)
	require.Equal(t, "45", aws.StringValue(out.Attributes["VisibilityTimeout"]))
	require.Contains(t, got.Header.Get("Authorization"), "Credential=ASIAX/")
	require.Contains(t, got.Header.Get("Authorization"), "/ap-southeast-2/sqs/aws4_request")
	require.Equal(t, "token", got.Header.Get("X-Amz-Security-Token"))
	require.Equal(t, "gw.example:9999", got.Host)
	require.Contains(t, body, `"QueueUrl":"https://gw.example:9999/123456789012/in"`)
}
