package projection

import (
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

// TestGoldenRemoveNAT_RetriesOnNoResponders pins the delete-nat retry-on-
// ErrNoResponders characteristic: a teardown published while vpcd is between
// subscribe calls is not dropped, it is retried until a responder appears or
// the retry budget (3 attempts, 500ms apart) is exhausted. The responder
// subscribes 700ms late, inside that budget, so RemoveNAT must still
// succeed, and it must do so well short of the 15s per-attempt timeout.
func TestGoldenRemoveNAT_RetriesOnNoResponders(t *testing.T) {
	_, nc := testutil.StartTestNATS(t)

	captured := make(chan []byte, 1)
	go func() {
		time.Sleep(700 * time.Millisecond)
		sub, err := nc.Subscribe("vpc.delete-nat", func(msg *nats.Msg) {
			captured <- msg.Data
			_ = msg.Respond([]byte(`{"success":true}`))
		})
		if err != nil {
			return
		}
		t.Cleanup(func() { _ = sub.Unsubscribe() })
	}()

	start := time.Now()
	New(nc).RemoveNAT("vpc-1", "203.0.113.5", "10.0.1.5", "port-eni-1", "02:00:00:00:00:01")
	elapsed := time.Since(start)

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
	_, nc := testutil.StartTestNATS(t)
	c := New(nc)

	t.Run("success", func(t *testing.T) {
		sub, err := nc.Subscribe("golden.ack.success", func(msg *nats.Msg) {
			_ = msg.Respond([]byte(`{"success":true}`))
		})
		require.NoError(t, err)
		t.Cleanup(func() { _ = sub.Unsubscribe() })

		err = c.request("golden.ack.success", map[string]string{"k": "v"}, time.Second)
		assert.NoError(t, err)
	})

	t.Run("error", func(t *testing.T) {
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
