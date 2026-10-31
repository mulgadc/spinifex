package jetstream_test

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/mulgadc/spinifex/internal/testkit"
	"github.com/mulgadc/spinifex/spinifex/domains/sqs/message"
	sqsjs "github.com/mulgadc/spinifex/spinifex/domains/sqs/message/jetstream"
	"github.com/mulgadc/spinifex/spinifex/domains/sqs/queue"
	"github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	acct   = "000000000001"
	other  = "000000000002"
	region = "us-east-1"
	sender = "AIDAEXAMPLESENDER"
)

var fixedNow = time.Date(2026, 10, 30, 12, 0, 0, 123_456_789, time.UTC)

type sendEnv struct {
	queues *queue.Store
	store  message.Store
}

func newSendEnv(t *testing.T) sendEnv {
	t.Helper()
	_, _, js := testutil.StartTestJetStream(t)
	ctx := context.Background()
	qs, err := queue.Open(ctx, js, 3, 2)
	require.NoError(t, err)
	return sendEnv{queues: qs, store: openStore(t, storeMaxPayload)}
}

// The dedicated store is its own server; ADR-0011:S4 requires its
// max_payload to admit a full message, which the spike showed at 2MB.
const storeMaxPayload = 2 << 20

func openStore(t *testing.T, maxPayload int32) message.Store {
	t.Helper()
	ms, _ := openStoreJS(t, maxPayload)
	return ms
}

func openStoreJS(t *testing.T, maxPayload int32) (message.Store, jetstream.JetStream) {
	t.Helper()
	ns, err := server.NewServer(&server.Options{
		Host: "127.0.0.1", Port: -1, JetStream: true, StoreDir: t.TempDir(),
		NoLog: true, NoSigs: true, MaxPayload: maxPayload,
	})
	require.NoError(t, err)
	go ns.Start()
	require.True(t, ns.ReadyForConnections(10*time.Second))
	t.Cleanup(ns.Shutdown)
	nc, err := nats.Connect(ns.ClientURL())
	require.NoError(t, err)
	t.Cleanup(nc.Close)
	js, err := jetstream.New(nc)
	require.NoError(t, err)
	ms, err := sqsjs.New(context.Background(), js, sqsjs.Config{MessageStreams: 3, StateBuckets: 2, DuplicateWindow: time.Minute})
	require.NoError(t, err)
	return ms, js
}

func (e sendEnv) sender(store message.Store) *message.Sender {
	if store == nil {
		store = e.store
	}
	return &message.Sender{Store: store, Queues: e.queues, Now: func() time.Time { return fixedNow }}
}

func (e sendEnv) create(t *testing.T, account, name string, attrs map[string]string) queue.Record {
	t.Helper()
	r, err := e.queues.Create(context.Background(), queue.CreateRequest{Account: account, Region: region, Name: name, Attributes: attrs})
	require.NoError(t, err)
	return r
}

func (e sendEnv) count(t *testing.T, r queue.Record) uint64 {
	t.Helper()
	u, err := e.store.Count(context.Background(), r.Namespace())
	require.NoError(t, err)
	return u.Messages
}

func input(name, body string) message.SendInput {
	return message.SendInput{Account: acct, Region: region, Queue: name, Sender: sender, Body: []byte(body)}
}

func ptr(n int) *int { return &n }

func TestSendAppendsBodyAndMetadata(t *testing.T) {
	e := newSendEnv(t)
	q := e.create(t, acct, "orders", nil)

	res, err := e.sender(nil).Send(context.Background(), input("orders", "hello"))
	require.NoError(t, err)
	assert.Equal(t, "5d41402abc4b2a76b9719d911017c592", res.MD5OfBody)
	assert.Regexp(t, uuidV4, res.MessageID)
	enq := fixedNow.Truncate(time.Millisecond)
	assert.Equal(t, enq, res.EnqueuedAt)
	assert.Equal(t, enq, res.VisibleAfter, "the queue's default delay is 0")

	got, err := e.store.Get(context.Background(), q.Namespace(), res.Position)
	require.NoError(t, err)
	assert.Equal(t, []byte("hello"), got.Body)
	ms := strconv.FormatInt(enq.UnixMilli(), 10)
	assert.Equal(t, map[string]string{
		message.MetaV: "1", message.MetaID: res.MessageID, message.MetaEnq: ms, message.MetaVis: ms,
		message.MetaSender: sender, message.MetaMD5: res.MD5OfBody, message.MetaAcct: acct,
	}, got.Meta)
}

var uuidV4 = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

// faultStore wraps a real store's Append.
type faultStore struct {
	message.Store

	append func(context.Context, message.Namespace, message.AppendInput) (message.AppendResult, error)
}

func (f faultStore) Append(ctx context.Context, ns message.Namespace, in message.AppendInput) (message.AppendResult, error) {
	return f.append(ctx, ns, in)
}

func TestSendLostAppendReplyIsNeverSuccess(t *testing.T) {
	e := newSendEnv(t)
	q := e.create(t, acct, "orders", nil)
	errLost := errors.New("reply lost")

	t.Run("applied but the reply was lost", func(t *testing.T) {
		fs := faultStore{Store: e.store, append: func(ctx context.Context, ns message.Namespace, in message.AppendInput) (message.AppendResult, error) {
			_, err := e.store.Append(ctx, ns, in)
			require.NoError(t, err)
			return message.AppendResult{}, errLost
		}}
		res, err := e.sender(fs).Send(context.Background(), input("orders", "x"))
		require.ErrorIs(t, err, message.ErrUnavailable)
		assert.Zero(t, res)
		assert.Equal(t, uint64(1), e.count(t, q), "the stored copy is a duplicate if the client retries")
	})
}

// Neither logs nor errors carry a body, whatever the outcome.
func TestSendNeverExposesTheBody(t *testing.T) {
	var logs bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	e := newSendEnv(t)
	e.create(t, acct, "small", map[string]string{"MaximumMessageSize": "1024"})
	const secret = "SECRET-BODY-7f3a"
	failing := faultStore{Store: e.store, append: func(context.Context, message.Namespace, message.AppendInput) (message.AppendResult, error) {
		return message.AppendResult{}, errors.New("boom")
	}}
	var errs []error
	for _, tc := range []struct {
		store message.Store
		body  string
	}{
		{nil, secret},
		{nil, secret + "\x00"},
		{nil, secret + strings.Repeat("a", 2000)},
		{failing, secret},
	} {
		_, err := e.sender(tc.store).Send(context.Background(), input("small", tc.body))
		errs = append(errs, err)
	}
	require.NoError(t, errs[0])
	for _, err := range errs[1:] {
		require.Error(t, err)
		assert.NotContains(t, err.Error(), secret)
	}
	assert.NotContains(t, logs.String(), secret)
}
