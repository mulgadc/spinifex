package projection

import (
	"context"
	"log/slog"
	"testing"
	"time"

	"github.com/mulgadc/spinifex/internal/testkit"
	"github.com/nats-io/nats.go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// This file characterizes the vpc.add-nat / vpc.delete-nat wire behaviour:
// the exact JSON AddNAT/RemoveNAT put on the wire, the {success,error} ack
// envelope the internal request helper expects, and the delete-nat
// retry-on-ErrNoResponders behaviour. in-package because request is
// unexported by design: this client has no generic Publish(topic, any).

// captureOnce subscribes to subject, replies with reply to the first message,
// and returns a channel that receives that message's raw data.
func captureOnce(t *testing.T, nc *nats.Conn, subject string, reply []byte) <-chan []byte {
	t.Helper()
	ch := make(chan []byte, 1)
	sub, err := nc.Subscribe(subject, func(msg *nats.Msg) {
		ch <- msg.Data
		if msg.Reply != "" {
			_ = msg.Respond(reply)
		}
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = sub.Unsubscribe() })
	return ch
}

func TestGoldenAddNAT_WireShape(t *testing.T) {
	t.Parallel()
	_, nc := testutil.StartTestNATS(t)
	captured := captureOnce(t, nc, "vpc.add-nat", []byte(`{"success":true}`))

	err := New(nc).AddNAT("vpc-1", "203.0.113.5", "10.0.1.5", "port-eni-1", "02:00:00:00:00:01")
	require.NoError(t, err)

	golden := `{"vpc_id":"vpc-1","external_ip":"203.0.113.5","logical_ip":"10.0.1.5","port_name":"port-eni-1","mac":"02:00:00:00:00:01"}`
	select {
	case data := <-captured:
		assert.JSONEq(t, golden, string(data))
	case <-time.After(2 * time.Second):
		t.Fatal("vpc.add-nat was never published")
	}
}

func TestGoldenAddNAT_ErrorSurfacesToCaller(t *testing.T) {
	t.Parallel()
	_, nc := testutil.StartTestNATS(t)
	sub, err := nc.Subscribe("vpc.add-nat", func(msg *nats.Msg) {
		_ = msg.Respond([]byte(`{"success":false,"error":"ovn commit failed"}`))
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = sub.Unsubscribe() })

	err = New(nc).AddNAT("vpc-1", "203.0.113.5", "10.0.1.5", "port-eni-1", "02:00:00:00:00:01")
	require.Error(t, err)
	assert.Equal(t, "vpc.add-nat: ovn commit failed", err.Error())
}

func TestGoldenRemoveNAT_WireShape(t *testing.T) {
	t.Parallel()
	_, nc := testutil.StartTestNATS(t)
	captured := captureOnce(t, nc, "vpc.delete-nat", []byte(`{"success":true}`))

	New(nc).RemoveNAT("vpc-1", "203.0.113.5", "10.0.1.5", "port-eni-1", "02:00:00:00:00:01")

	golden := `{"vpc_id":"vpc-1","external_ip":"203.0.113.5","logical_ip":"10.0.1.5","port_name":"port-eni-1","mac":"02:00:00:00:00:01"}`
	select {
	case data := <-captured:
		assert.JSONEq(t, golden, string(data))
	case <-time.After(2 * time.Second):
		t.Fatal("vpc.delete-nat was never published")
	}
}

// retryHook is a slog handler that calls onAttempt with the attempt number of
// each delete-nat no-responders warning, which removeNAT logs before it waits.
type retryHook struct{ onAttempt func(int) }

func (h retryHook) Enabled(context.Context, slog.Level) bool {
	return true
}

func (h retryHook) WithAttrs([]slog.Attr) slog.Handler {
	return h
}

func (h retryHook) WithGroup(string) slog.Handler {
	return h
}

func (h retryHook) Handle(_ context.Context, r slog.Record) error {
	if r.Message != "vpc.delete-nat has no subscriber; retrying" {
		return nil
	}
	r.Attrs(func(a slog.Attr) bool {
		if a.Key == "attempt" {
			h.onAttempt(int(a.Value.Int64()))
			return false
		}
		return true
	})
	return nil
}

// TestGoldenRemoveNAT_RetriesOnNoResponders pins the delete-nat retry-on-
// ErrNoResponders characteristic: a teardown published while vpcd is between
// subscribe calls is not dropped, it is retried until a responder appears or
// the retry budget (3 attempts) is exhausted. The responder subscribes only
// after the second attempt finds nobody, so the third attempt must deliver,
// and RemoveNAT must finish well short of the 15s per-attempt timeout.
func TestGoldenRemoveNAT_RetriesOnNoResponders(t *testing.T) {
	shortenDeleteNATRetryDelay(t, 10*time.Millisecond)
	_, nc := testutil.StartTestNATS(t)

	captured := make(chan []byte, 1)
	var attempts []int
	prev := slog.Default()
	slog.SetDefault(slog.New(retryHook{onAttempt: func(n int) {
		attempts = append(attempts, n)
		if n != 2 {
			return
		}
		sub, err := nc.Subscribe("vpc.delete-nat", func(msg *nats.Msg) {
			captured <- msg.Data
			_ = msg.Respond([]byte(`{"success":true}`))
		})
		require.NoError(t, err)
		t.Cleanup(func() { _ = sub.Unsubscribe() })
		require.NoError(t, nc.Flush())
	}}))
	t.Cleanup(func() { slog.SetDefault(prev) })

	start := time.Now()
	New(nc).RemoveNAT("vpc-1", "203.0.113.5", "10.0.1.5", "port-eni-1", "02:00:00:00:00:01")
	elapsed := time.Since(start)

	assert.Equal(t, []int{1, 2}, attempts,
		"the first two attempts must find no responder and be retried")
	assert.Less(t, elapsed, 5*time.Second,
		"retry-on-ErrNoResponders must fail fast and retry, not block on the 15s per-attempt timeout")

	select {
	case data := <-captured:
		assert.JSONEq(t,
			`{"vpc_id":"vpc-1","external_ip":"203.0.113.5","logical_ip":"10.0.1.5","port_name":"port-eni-1","mac":"02:00:00:00:00:01"}`,
			string(data))
	case <-time.After(2 * time.Second):
		t.Fatal("vpc.delete-nat was never delivered to the late responder")
	}
}

func TestGoldenRequest_AckEnvelope(t *testing.T) {
	t.Parallel()
	_, nc := testutil.StartTestNATS(t)
	c := New(nc)

	t.Run("success", func(t *testing.T) {
		t.Parallel()
		sub, err := nc.Subscribe("golden.ack.success", func(msg *nats.Msg) {
			_ = msg.Respond([]byte(`{"success":true}`))
		})
		require.NoError(t, err)
		t.Cleanup(func() { _ = sub.Unsubscribe() })

		err = c.request("golden.ack.success", map[string]string{"k": "v"}, time.Second)
		assert.NoError(t, err)
	})

	t.Run("error", func(t *testing.T) {
		t.Parallel()
		sub, err := nc.Subscribe("golden.ack.error", func(msg *nats.Msg) {
			_ = msg.Respond([]byte(`{"success":false,"error":"boom"}`))
		})
		require.NoError(t, err)
		t.Cleanup(func() { _ = sub.Unsubscribe() })

		err = c.request("golden.ack.error", map[string]string{"k": "v"}, time.Second)
		require.Error(t, err)
		assert.Equal(t, "golden.ack.error: boom", err.Error())
	})
}

// TestGoldenRequest_MarshalsExactPayload pins that request puts the caller's
// value on the wire verbatim, with no envelope wrapping on the request side
// (only the reply is enveloped).
func TestGoldenRequest_MarshalsExactPayload(t *testing.T) {
	t.Parallel()
	_, nc := testutil.StartTestNATS(t)
	captured := captureOnce(t, nc, "golden.payload", []byte(`{"success":true}`))

	type payload struct {
		VpcId string `json:"vpc_id"`
	}
	require.NoError(t, New(nc).request("golden.payload", payload{VpcId: "vpc-1"}, time.Second))

	select {
	case data := <-captured:
		assert.JSONEq(t, `{"vpc_id":"vpc-1"}`, string(data))
	case <-time.After(2 * time.Second):
		t.Fatal("golden.payload was never published")
	}
}
