package daemon

import (
	"encoding/json"
	"testing"
	"time"

	handlers_elbv2 "github.com/mulgadc/spinifex/spinifex/handlers/elbv2"
	"github.com/nats-io/nats.go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSplitSubjectTail(t *testing.T) {
	cases := []struct {
		name    string
		subject string
		prefix  string
		want    string
	}{
		{"happy", "system.TerminateInstance.i-abc", "system.TerminateInstance.", "i-abc"},
		{"empty tail equals prefix length", "system.TerminateInstance.", "system.TerminateInstance.", ""},
		{"prefix mismatch", "other.subject", "system.TerminateInstance.", ""},
		{"shorter than prefix", "sys", "system.TerminateInstance.", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, splitSubjectTail(tc.subject, tc.prefix))
		})
	}
}

func TestRespondWithSystemLaunchOutput(t *testing.T) {
	nc, err := nats.Connect(sharedNATSURL)
	require.NoError(t, err)
	t.Cleanup(nc.Close)

	replyCh := make(chan []byte, 1)
	sub, err := nc.Subscribe("test.launch.ok.reply", func(msg *nats.Msg) {
		replyCh <- msg.Data
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = sub.Unsubscribe() })

	msg := &nats.Msg{Subject: "system.LaunchInstance.sys.micro", Reply: "test.launch.ok.reply", Sub: sub}
	out := &handlers_elbv2.SystemInstanceOutput{InstanceID: "i-abc", PrivateIP: "10.0.0.5"}
	msg = msgWithConn(nc, msg)

	respondWithSystemLaunchOutput(msg, out)

	select {
	case payload := <-replyCh:
		var env struct {
			Output *handlers_elbv2.SystemInstanceOutput `json:"output,omitempty"`
			Error  string                               `json:"error,omitempty"`
		}
		require.NoError(t, json.Unmarshal(payload, &env))
		require.NotNil(t, env.Output)
		assert.Equal(t, "i-abc", env.Output.InstanceID)
		assert.Empty(t, env.Error)
	case <-time.After(2 * time.Second):
		t.Fatal("expected response on reply subject")
	}
}

func TestRespondWithSystemLaunchError(t *testing.T) {
	nc, err := nats.Connect(sharedNATSURL)
	require.NoError(t, err)
	t.Cleanup(nc.Close)

	replyCh := make(chan []byte, 1)
	sub, err := nc.Subscribe("test.launch.err.reply", func(msg *nats.Msg) {
		replyCh <- msg.Data
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = sub.Unsubscribe() })

	msg := msgWithConn(nc, &nats.Msg{Subject: "system.LaunchInstance.sys.micro", Reply: "test.launch.err.reply", Sub: sub})

	respondWithSystemLaunchError(msg, "boom")

	select {
	case payload := <-replyCh:
		var env struct {
			Error string `json:"error,omitempty"`
		}
		require.NoError(t, json.Unmarshal(payload, &env))
		assert.Equal(t, "boom", env.Error)
	case <-time.After(2 * time.Second):
		t.Fatal("expected error response on reply subject")
	}
}

func TestRespondWithSystemLaunchError_EmptyDefaultsToServerInternal(t *testing.T) {
	nc, err := nats.Connect(sharedNATSURL)
	require.NoError(t, err)
	t.Cleanup(nc.Close)

	replyCh := make(chan []byte, 1)
	sub, err := nc.Subscribe("test.launch.empty.reply", func(msg *nats.Msg) {
		replyCh <- msg.Data
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = sub.Unsubscribe() })

	msg := msgWithConn(nc, &nats.Msg{Subject: "system.LaunchInstance.sys.micro", Reply: "test.launch.empty.reply", Sub: sub})

	respondWithSystemLaunchError(msg, "")

	select {
	case payload := <-replyCh:
		var env struct {
			Error string `json:"error,omitempty"`
		}
		require.NoError(t, json.Unmarshal(payload, &env))
		assert.NotEmpty(t, env.Error, "empty input must be replaced with default error")
	case <-time.After(2 * time.Second):
		t.Fatal("expected response on reply subject")
	}
}

func TestRespondWithSystemTerminateOK(t *testing.T) {
	nc, err := nats.Connect(sharedNATSURL)
	require.NoError(t, err)
	t.Cleanup(nc.Close)

	replyCh := make(chan []byte, 1)
	sub, err := nc.Subscribe("test.term.ok.reply", func(msg *nats.Msg) {
		replyCh <- msg.Data
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = sub.Unsubscribe() })

	msg := msgWithConn(nc, &nats.Msg{Subject: "system.TerminateInstance.i-x", Reply: "test.term.ok.reply", Sub: sub})

	respondWithSystemTerminateOK(msg)

	select {
	case payload := <-replyCh:
		var env struct {
			Error string `json:"error,omitempty"`
		}
		require.NoError(t, json.Unmarshal(payload, &env))
		assert.Empty(t, env.Error)
	case <-time.After(2 * time.Second):
		t.Fatal("expected response on reply subject")
	}
}

func TestRespondWithSystemTerminateError(t *testing.T) {
	nc, err := nats.Connect(sharedNATSURL)
	require.NoError(t, err)
	t.Cleanup(nc.Close)

	replyCh := make(chan []byte, 1)
	sub, err := nc.Subscribe("test.term.err.reply", func(msg *nats.Msg) {
		replyCh <- msg.Data
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = sub.Unsubscribe() })

	msg := msgWithConn(nc, &nats.Msg{Subject: "system.TerminateInstance.i-x", Reply: "test.term.err.reply", Sub: sub})

	respondWithSystemTerminateError(msg, "not found")

	select {
	case payload := <-replyCh:
		var env struct {
			Error string `json:"error,omitempty"`
		}
		require.NoError(t, json.Unmarshal(payload, &env))
		assert.Equal(t, "not found", env.Error)
	case <-time.After(2 * time.Second):
		t.Fatal("expected response on reply subject")
	}
}

func TestRespondWithSystemTerminateError_EmptyDefaultsToServerInternal(t *testing.T) {
	nc, err := nats.Connect(sharedNATSURL)
	require.NoError(t, err)
	t.Cleanup(nc.Close)

	replyCh := make(chan []byte, 1)
	sub, err := nc.Subscribe("test.term.empty.reply", func(msg *nats.Msg) {
		replyCh <- msg.Data
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = sub.Unsubscribe() })

	msg := msgWithConn(nc, &nats.Msg{Subject: "system.TerminateInstance.i-x", Reply: "test.term.empty.reply", Sub: sub})

	respondWithSystemTerminateError(msg, "")

	select {
	case payload := <-replyCh:
		var env struct {
			Error string `json:"error,omitempty"`
		}
		require.NoError(t, json.Unmarshal(payload, &env))
		assert.NotEmpty(t, env.Error)
	case <-time.After(2 * time.Second):
		t.Fatal("expected response on reply subject")
	}
}

func TestHandleSystemLaunchInstance_InvalidJSON(t *testing.T) {
	nc, err := nats.Connect(sharedNATSURL)
	require.NoError(t, err)
	t.Cleanup(nc.Close)

	d := &Daemon{natsConn: nc, natsSubscriptions: make(map[string]*nats.Subscription)}

	replyCh := make(chan []byte, 1)
	sub, err := nc.Subscribe("test.launch.bad.reply", func(msg *nats.Msg) {
		replyCh <- msg.Data
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = sub.Unsubscribe() })

	msg := msgWithConn(nc, &nats.Msg{
		Subject: "system.LaunchInstance.sys.micro",
		Reply:   "test.launch.bad.reply",
		Sub:     sub,
		Data:    []byte("{not valid"),
	})

	d.handleSystemLaunchInstance(msg)

	select {
	case payload := <-replyCh:
		var env struct {
			Error string `json:"error,omitempty"`
		}
		require.NoError(t, json.Unmarshal(payload, &env))
		assert.NotEmpty(t, env.Error)
	case <-time.After(2 * time.Second):
		t.Fatal("expected error response for invalid JSON")
	}
}

func TestHandleSystemTerminateInstance_MissingID(t *testing.T) {
	nc, err := nats.Connect(sharedNATSURL)
	require.NoError(t, err)
	t.Cleanup(nc.Close)

	d := &Daemon{natsConn: nc, natsSubscriptions: make(map[string]*nats.Subscription)}

	replyCh := make(chan []byte, 1)
	sub, err := nc.Subscribe("test.term.missing.reply", func(msg *nats.Msg) {
		replyCh <- msg.Data
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = sub.Unsubscribe() })

	msg := msgWithConn(nc, &nats.Msg{
		Subject: "system.TerminateInstance.",
		Reply:   "test.term.missing.reply",
		Sub:     sub,
	})

	d.handleSystemTerminateInstance(msg)

	select {
	case payload := <-replyCh:
		var env struct {
			Error string `json:"error,omitempty"`
		}
		require.NoError(t, json.Unmarshal(payload, &env))
		assert.NotEmpty(t, env.Error)
	case <-time.After(2 * time.Second):
		t.Fatal("expected error response for missing instance ID")
	}
}

func TestSubscribeSystemTerminate_NilConn(t *testing.T) {
	d := &Daemon{natsSubscriptions: make(map[string]*nats.Subscription)}
	assert.NoError(t, d.subscribeSystemTerminate("i-noop"), "nil natsConn must short-circuit without error")
	assert.Empty(t, d.natsSubscriptions, "no subscription should be registered when conn is nil")
}

func TestSubscribeSystemTerminate_Idempotent(t *testing.T) {
	nc, err := nats.Connect(sharedNATSURL)
	require.NoError(t, err)
	t.Cleanup(nc.Close)

	d := &Daemon{natsConn: nc, natsSubscriptions: make(map[string]*nats.Subscription)}

	require.NoError(t, d.subscribeSystemTerminate("i-idem"))
	first := d.natsSubscriptions["system.TerminateInstance.i-idem"]
	require.NotNil(t, first)

	require.NoError(t, d.subscribeSystemTerminate("i-idem"))
	second := d.natsSubscriptions["system.TerminateInstance.i-idem"]
	assert.Same(t, first, second, "second call must be a no-op and keep the original subscription")
}

// msgWithConn returns msg with its internal connection set so msg.Respond
// publishes through nc. nats.Msg.Respond requires a backing connection set
// either via Subscribe delivery or by hand for synthetic test messages.
func msgWithConn(nc *nats.Conn, msg *nats.Msg) *nats.Msg {
	// The Subscribe-created subscription embeds the conn; reuse it.
	if msg.Sub != nil {
		return msg
	}
	sub, _ := nc.Subscribe("_test.unused.subject."+msg.Reply, func(*nats.Msg) {})
	msg.Sub = sub
	return msg
}
