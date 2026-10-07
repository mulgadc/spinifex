package daemon

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/mulgadc/spinifex/spinifex/awserrors"
	handlers_elbv2 "github.com/mulgadc/spinifex/spinifex/handlers/elbv2"
	"github.com/mulgadc/spinifex/spinifex/handlers/sysinstance"
	"github.com/mulgadc/spinifex/spinifex/vm"
	"github.com/nats-io/nats.go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

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

func TestLaunchSystemInstanceOnNode_RemoteRoundTrip(t *testing.T) {
	nc, err := nats.Connect(sharedNATSURL)
	require.NoError(t, err)
	t.Cleanup(nc.Close)

	d := &Daemon{natsConn: nc, node: "node-a", natsSubscriptions: make(map[string]*nats.Subscription)}

	// Stand in for the remote node's owning daemon on the node-targeted subject.
	gotCh := make(chan *handlers_elbv2.SystemInstanceInput, 1)
	sub, err := nc.Subscribe("system.LaunchInstance.sys.medium.node-b", func(msg *nats.Msg) {
		var in handlers_elbv2.SystemInstanceInput
		_ = json.Unmarshal(msg.Data, &in)
		gotCh <- &in
		payload, _ := json.Marshal(systemInstanceLaunchEnvelope{
			Output: &handlers_elbv2.SystemInstanceOutput{InstanceID: "i-remote", PrivateIP: "10.0.2.7"},
		})
		_ = msg.Respond(payload)
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = sub.Unsubscribe() })

	out, err := d.LaunchSystemInstanceOnNode("node-b", &handlers_elbv2.SystemInstanceInput{
		InstanceType: "sys.medium", ImageID: "ami-x",
	})
	require.NoError(t, err)
	require.NotNil(t, out)
	assert.Equal(t, "i-remote", out.InstanceID)

	select {
	case in := <-gotCh:
		assert.Equal(t, "sys.medium", in.InstanceType, "node-targeted launch carries the CP instance type")
	case <-time.After(2 * time.Second):
		t.Fatal("remote node never received the node-targeted launch")
	}
}

func TestLaunchSystemInstanceOnNode_RemoteErrorPropagated(t *testing.T) {
	nc, err := nats.Connect(sharedNATSURL)
	require.NoError(t, err)
	t.Cleanup(nc.Close)

	d := &Daemon{natsConn: nc, node: "node-a", natsSubscriptions: make(map[string]*nats.Subscription)}

	sub, err := nc.Subscribe("system.LaunchInstance.sys.medium.node-b", func(msg *nats.Msg) {
		payload, _ := json.Marshal(systemInstanceLaunchEnvelope{Error: "insufficient capacity"})
		_ = msg.Respond(payload)
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = sub.Unsubscribe() })

	_, err = d.LaunchSystemInstanceOnNode("node-b", &handlers_elbv2.SystemInstanceInput{InstanceType: "sys.medium"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "insufficient capacity")
}

func TestLaunchSystemInstanceOnNode_RemoteWithoutConn(t *testing.T) {
	d := &Daemon{node: "node-a"}
	_, err := d.LaunchSystemInstanceOnNode("node-b", &handlers_elbv2.SystemInstanceInput{InstanceType: "sys.medium"})
	require.Error(t, err, "remote placement requires a NATS connection")
}

// TestTerminateSystemInstanceRemote_RoundTrip locks the contract that terminating
// a CP VM this node does not own routes over system.TerminateInstance.{id} to the
// owning daemon, which stops qemu and cascade-deletes the ENI before replying —
// so the cluster-wide teardown actually frees the remote ENI instead of deleting
// it while still attached (InvalidNetworkInterface.InUse).
func TestTerminateSystemInstanceRemote_RoundTrip(t *testing.T) {
	nc, err := nats.Connect(sharedNATSURL)
	require.NoError(t, err)
	t.Cleanup(nc.Close)

	d := &Daemon{natsConn: nc, node: "node-a", natsSubscriptions: make(map[string]*nats.Subscription)}

	gotCh := make(chan struct{}, 1)
	sub, err := nc.Subscribe("system.TerminateInstance.i-remote", func(msg *nats.Msg) {
		gotCh <- struct{}{}
		payload, _ := json.Marshal(systemInstanceTerminateEnvelope{})
		_ = msg.Respond(payload)
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = sub.Unsubscribe() })

	require.NoError(t, d.terminateSystemInstanceRemote("i-remote"))

	select {
	case <-gotCh:
	case <-time.After(2 * time.Second):
		t.Fatal("owning node never received the routed terminate")
	}
}

// TestTerminateSystemInstanceRemote_NoResponders: no node owns the VM, so the
// request gets no responders. That means the VM is genuinely gone, reported as
// ErrSystemInstanceNotFound — which TerminateK3sServerVM treats as idempotent.
func TestTerminateSystemInstanceRemote_NoResponders(t *testing.T) {
	nc, err := nats.Connect(sharedNATSURL)
	require.NoError(t, err)
	t.Cleanup(nc.Close)

	d := &Daemon{natsConn: nc, node: "node-a", natsSubscriptions: make(map[string]*nats.Subscription)}

	err = d.terminateSystemInstanceRemote("i-orphan")
	require.Error(t, err)
	assert.ErrorIs(t, err, sysinstance.ErrSystemInstanceNotFound,
		"no owner means the VM is gone — caller must see idempotent NotFound, not a hang")
}

// TestTerminateSystemInstanceRemote_NotFoundPropagated: the owner replies with a
// NotFound (it raced to gone); the routing layer normalizes it back to a typed
// ErrSystemInstanceNotFound so the teardown still treats it as idempotent.
func TestTerminateSystemInstanceRemote_NotFoundPropagated(t *testing.T) {
	nc, err := nats.Connect(sharedNATSURL)
	require.NoError(t, err)
	t.Cleanup(nc.Close)

	d := &Daemon{natsConn: nc, node: "node-a", natsSubscriptions: make(map[string]*nats.Subscription)}

	sub, err := nc.Subscribe("system.TerminateInstance.i-gone", func(msg *nats.Msg) {
		payload, _ := json.Marshal(systemInstanceTerminateEnvelope{
			Error: sysinstance.ErrSystemInstanceNotFound.Error() + ": i-gone",
		})
		_ = msg.Respond(payload)
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = sub.Unsubscribe() })

	err = d.terminateSystemInstanceRemote("i-gone")
	require.Error(t, err)
	assert.ErrorIs(t, err, sysinstance.ErrSystemInstanceNotFound)
}

// TestTerminateSystemInstanceRemote_ErrorPropagated: a real teardown failure on
// the owner surfaces to the caller (not swallowed), so the cluster delete fails
// loudly and the backstop reaper re-drives it rather than stranding billable infra.
func TestTerminateSystemInstanceRemote_ErrorPropagated(t *testing.T) {
	nc, err := nats.Connect(sharedNATSURL)
	require.NoError(t, err)
	t.Cleanup(nc.Close)

	d := &Daemon{natsConn: nc, node: "node-a", natsSubscriptions: make(map[string]*nats.Subscription)}

	sub, err := nc.Subscribe("system.TerminateInstance.i-stuck", func(msg *nats.Msg) {
		payload, _ := json.Marshal(systemInstanceTerminateEnvelope{Error: "volume detach timed out"})
		_ = msg.Respond(payload)
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = sub.Unsubscribe() })

	err = d.terminateSystemInstanceRemote("i-stuck")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "volume detach timed out")
	assert.NotErrorIs(t, err, sysinstance.ErrSystemInstanceNotFound,
		"a real failure must not be misreported as idempotent NotFound")
}

// TestTerminateSystemInstanceRemote_WithoutConn: with no NATS connection there is
// no way to reach an owner, so the VM is unreachable — reported as NotFound.
func TestTerminateSystemInstanceRemote_WithoutConn(t *testing.T) {
	d := &Daemon{node: "node-a"}
	err := d.terminateSystemInstanceRemote("i-x")
	require.Error(t, err)
	assert.ErrorIs(t, err, sysinstance.ErrSystemInstanceNotFound)
}

// TestHandleSystemLaunchInstance_PanicRecovered drives a valid launch against a
// daemon with no resourceMgr so LaunchSystemInstance nil-derefs. The per-request
// goroutine must recover the panic, reply with an error, and drain the dispatch
// WaitGroup — a panic in a detached launch must never crash the daemon (267.4).
func TestHandleSystemLaunchInstance_PanicRecovered(t *testing.T) {
	nc, err := nats.Connect(sharedNATSURL)
	require.NoError(t, err)
	t.Cleanup(nc.Close)

	d := &Daemon{natsConn: nc, natsSubscriptions: make(map[string]*nats.Subscription)}

	replyCh := make(chan []byte, 1)
	sub, err := nc.Subscribe("test.launch.panic.reply", func(msg *nats.Msg) { replyCh <- msg.Data })
	require.NoError(t, err)
	t.Cleanup(func() { _ = sub.Unsubscribe() })

	payload, err := json.Marshal(&handlers_elbv2.SystemInstanceInput{InstanceType: "sys.medium"})
	require.NoError(t, err)
	msg := msgWithConn(nc, &nats.Msg{
		Subject: "system.LaunchInstance.sys.medium",
		Reply:   "test.launch.panic.reply",
		Sub:     sub,
		Data:    payload,
	})

	d.handleSystemLaunchInstance(msg)

	select {
	case data := <-replyCh:
		var env struct {
			Error string `json:"error,omitempty"`
		}
		require.NoError(t, json.Unmarshal(data, &env))
		assert.NotEmpty(t, env.Error, "recovered panic must reply with an error envelope")
	case <-time.After(2 * time.Second):
		t.Fatal("expected error response after recovered panic")
	}

	drained := make(chan struct{})
	go func() { d.systemDispatchWg.Wait(); close(drained) }()
	select {
	case <-drained:
	case <-time.After(2 * time.Second):
		t.Fatal("systemDispatchWg did not drain after handler returned")
	}
}

// TestHandleSystemLaunchInstance_ConcurrentDispatch fires several launch
// requests at once and asserts each gets its own reply — the per-request
// goroutine model must not serialize requests on a single subscription
// goroutine (the head-of-line block this fix removes).
func TestHandleSystemLaunchInstance_ConcurrentDispatch(t *testing.T) {
	nc, err := nats.Connect(sharedNATSURL)
	require.NoError(t, err)
	t.Cleanup(nc.Close)

	d := &Daemon{natsConn: nc, natsSubscriptions: make(map[string]*nats.Subscription)}

	const n = 8
	replyCh := make(chan []byte, n)
	sub, err := nc.Subscribe("test.launch.concurrent.reply", func(msg *nats.Msg) { replyCh <- msg.Data })
	require.NoError(t, err)
	t.Cleanup(func() { _ = sub.Unsubscribe() })

	for i := range n {
		msg := msgWithConn(nc, &nats.Msg{
			Subject: "system.LaunchInstance.sys.medium",
			Reply:   "test.launch.concurrent.reply",
			Sub:     sub,
			Data:    []byte("{not valid"),
		})
		_ = i
		d.handleSystemLaunchInstance(msg)
	}

	got := 0
	for got < n {
		select {
		case <-replyCh:
			got++
		case <-time.After(3 * time.Second):
			t.Fatalf("expected %d replies, got %d", n, got)
		}
	}

	drained := make(chan struct{})
	go func() { d.systemDispatchWg.Wait(); close(drained) }()
	select {
	case <-drained:
	case <-time.After(2 * time.Second):
		t.Fatal("systemDispatchWg did not drain")
	}
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

// syncReply returns a message whose reply lands on a synchronous inbox, so a
// handler called directly can be asserted on without a responder goroutine.
func syncReply(t *testing.T, nc *nats.Conn, subject string, data []byte) (*nats.Msg, *nats.Subscription) {
	t.Helper()
	inbox := nats.NewInbox()
	sub, err := nc.SubscribeSync(inbox)
	require.NoError(t, err)
	t.Cleanup(func() { _ = sub.Unsubscribe() })
	return &nats.Msg{Subject: subject, Reply: inbox, Sub: sub, Data: data}, sub
}

func nextEnvelopeError(t *testing.T, sub *nats.Subscription) string {
	t.Helper()
	reply, err := sub.NextMsg(2 * time.Second)
	require.NoError(t, err)
	var env struct {
		Error string `json:"error,omitempty"`
	}
	require.NoError(t, json.Unmarshal(reply.Data, &env))
	return env.Error
}

func TestServeSystemLaunchInstance_UnknownType(t *testing.T) {
	nc, err := nats.Connect(sharedNATSURL)
	require.NoError(t, err)
	t.Cleanup(nc.Close)
	rm, err := NewResourceManager(nil, nil, nil)
	require.NoError(t, err)

	d := &Daemon{natsConn: nc, resourceMgr: rm, natsSubscriptions: make(map[string]*nats.Subscription)}
	msg, sub := syncReply(t, nc, "system.LaunchInstance.zz9.none", []byte(`{"instance_type":"zz9.none"}`))

	assert.Equal(t, outcomeError, d.serveSystemLaunchInstance(msg))
	assert.Contains(t, nextEnvelopeError(t, sub), "unknown instance type: zz9.none")
	assert.Empty(t, d.natsSubscriptions, "a failed launch must not subscribe a terminate subject")
}

func TestLaunchSystemInstanceOnNode_Failures(t *testing.T) {
	nc, err := nats.Connect(sharedNATSURL)
	require.NoError(t, err)
	t.Cleanup(nc.Close)
	rm, err := NewResourceManager(nil, nil, nil)
	require.NoError(t, err)
	d := &Daemon{natsConn: nc, node: "node-a", resourceMgr: rm}

	t.Run("local target validates the type", func(t *testing.T) {
		_, err := d.LaunchSystemInstanceOnNode("node-a", &handlers_elbv2.SystemInstanceInput{InstanceType: "zz9.none"})
		assert.ErrorContains(t, err, "unknown instance type")
	})

	t.Run("remote without a type", func(t *testing.T) {
		_, err := d.LaunchSystemInstanceOnNode("node-b", &handlers_elbv2.SystemInstanceInput{})
		assert.ErrorContains(t, err, "missing InstanceType")
		_, err = d.LaunchSystemInstanceOnNode("node-b", nil)
		assert.ErrorContains(t, err, "missing InstanceType")
	})

	t.Run("remote node not listening", func(t *testing.T) {
		_, err := d.LaunchSystemInstanceOnNode("node-gone", &handlers_elbv2.SystemInstanceInput{InstanceType: "sys.p1"})
		assert.ErrorIs(t, err, nats.ErrNoResponders)
	})

	replies := map[string]string{
		"undecodable reply": "not json",
		"empty envelope":    "{}",
	}
	wantErr := map[string]string{
		"undecodable reply": "decode launch reply",
		"empty envelope":    "missing output payload",
	}
	for name, body := range replies {
		t.Run(name, func(t *testing.T) {
			node := "node-" + strings.ReplaceAll(name, " ", "-")
			sub, err := nc.Subscribe("system.LaunchInstance.sys.p1."+node, func(msg *nats.Msg) {
				_ = msg.Respond([]byte(body))
			})
			require.NoError(t, err)
			t.Cleanup(func() { _ = sub.Unsubscribe() })

			_, err = d.LaunchSystemInstanceOnNode(node, &handlers_elbv2.SystemInstanceInput{InstanceType: "sys.p1"})
			assert.ErrorContains(t, err, wantErr[name])
		})
	}
}

func TestTerminateSystemInstanceRemote_TransportFailures(t *testing.T) {
	t.Run("closed connection is not reported as already gone", func(t *testing.T) {
		nc, err := nats.Connect(sharedNATSURL)
		require.NoError(t, err)
		nc.Close()

		err = (&Daemon{natsConn: nc}).terminateSystemInstanceRemote("i-closed")
		require.ErrorContains(t, err, "route terminate i-closed")
		assert.NotErrorIs(t, err, sysinstance.ErrSystemInstanceNotFound)
	})

	t.Run("undecodable reply", func(t *testing.T) {
		nc, err := nats.Connect(sharedNATSURL)
		require.NoError(t, err)
		t.Cleanup(nc.Close)
		sub, err := nc.Subscribe("system.TerminateInstance.i-garbled", func(msg *nats.Msg) {
			_ = msg.Respond([]byte("garbled"))
		})
		require.NoError(t, err)
		t.Cleanup(func() { _ = sub.Unsubscribe() })

		err = (&Daemon{natsConn: nc}).terminateSystemInstanceRemote("i-garbled")
		assert.ErrorContains(t, err, "decode routed terminate reply i-garbled")
	})
}

func TestSubscribeSystemTerminate_ConnFailures(t *testing.T) {
	d := &Daemon{natsSubscriptions: make(map[string]*nats.Subscription)}
	require.NoError(t, d.subscribeSystemTerminateLocked("i-nil"), "nil natsConn must short-circuit")

	nc, err := nats.Connect(sharedNATSURL)
	require.NoError(t, err)
	nc.Close()
	d.natsConn = nc
	require.ErrorContains(t, d.subscribeSystemTerminate("i-closed"), "subscribe system.TerminateInstance.i-closed")
	assert.Empty(t, d.natsSubscriptions)
}

func TestHandleSystemTerminateInstance_Errors(t *testing.T) {
	nc, err := nats.Connect(sharedNATSURL)
	require.NoError(t, err)
	t.Cleanup(nc.Close)

	t.Run("unknown instance", func(t *testing.T) {
		d := &Daemon{natsConn: nc, vmMgr: vm.NewManager(), natsSubscriptions: make(map[string]*nats.Subscription)}
		msg, sub := syncReply(t, nc, "system.TerminateInstance.i-unknown", nil)
		d.serveSystemTerminateInstance(msg)
		assert.Contains(t, nextEnvelopeError(t, sub), sysinstance.ErrSystemInstanceNotFound.Error())
	})

	// No VM manager makes the terminate nil-deref; the dispatch goroutine must
	// recover and still answer.
	t.Run("panic is recovered", func(t *testing.T) {
		d := &Daemon{natsConn: nc, natsSubscriptions: make(map[string]*nats.Subscription)}
		msg, sub := syncReply(t, nc, "system.TerminateInstance.i-panic", nil)
		d.handleSystemTerminateInstance(msg)
		assert.Equal(t, awserrors.ErrorServerInternal, nextEnvelopeError(t, sub))
		d.systemDispatchWg.Wait()
	})
}
