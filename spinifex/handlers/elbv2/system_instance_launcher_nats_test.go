package handlers_elbv2

import (
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/mulgadc/spinifex/spinifex/testutil"
	"github.com/nats-io/nats.go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// stubResponder subscribes to a subject and replies with the provided
// envelope payload. Captures the last request payload for assertions.
type stubResponder struct {
	mu        sync.Mutex
	lastInput SystemInstanceInput
	called    int
	sub       *nats.Subscription
}

func (s *stubResponder) close() {
	if s.sub != nil {
		_ = s.sub.Unsubscribe()
	}
}

func TestNATSSystemInstanceLauncher_LaunchRoundTrip(t *testing.T) {
	_, nc, _ := testutil.StartTestJetStream(t)
	defer nc.Close()

	resp := &stubResponder{}
	sub, err := nc.Subscribe("system.LaunchInstance.sys.micro", func(msg *nats.Msg) {
		resp.mu.Lock()
		resp.called++
		_ = json.Unmarshal(msg.Data, &resp.lastInput)
		resp.mu.Unlock()

		envelope := struct {
			Output *SystemInstanceOutput `json:"output,omitempty"`
			Error  string                `json:"error,omitempty"`
		}{
			Output: &SystemInstanceOutput{
				InstanceID: "i-natsdispatched",
				PrivateIP:  "10.0.1.42",
			},
		}
		payload, _ := json.Marshal(envelope)
		_ = msg.Respond(payload)
	})
	require.NoError(t, err)
	resp.sub = sub
	defer resp.close()

	launcher := NewNATSSystemInstanceLauncher(nc, 5*time.Second)
	out, err := launcher.LaunchSystemInstance(&SystemInstanceInput{
		InstanceType: "sys.micro",
		SubnetID:     "subnet-test",
		ENIID:        "eni-test",
		Scheme:       SchemeInternal,
		AccountID:    "000000000001",
	})
	require.NoError(t, err)
	require.NotNil(t, out)
	assert.Equal(t, "i-natsdispatched", out.InstanceID)
	assert.Equal(t, "10.0.1.42", out.PrivateIP)

	resp.mu.Lock()
	defer resp.mu.Unlock()
	assert.Equal(t, 1, resp.called, "responder must be hit exactly once")
	assert.Equal(t, "sys.micro", resp.lastInput.InstanceType, "request payload must round trip JSON-encoded")
	assert.Equal(t, "subnet-test", resp.lastInput.SubnetID)
	assert.Equal(t, "eni-test", resp.lastInput.ENIID)
}

func TestNATSSystemInstanceLauncher_LaunchPropagatesRemoteError(t *testing.T) {
	_, nc, _ := testutil.StartTestJetStream(t)
	defer nc.Close()

	sub, err := nc.Subscribe("system.LaunchInstance.sys.micro", func(msg *nats.Msg) {
		envelope := struct {
			Output *SystemInstanceOutput `json:"output,omitempty"`
			Error  string                `json:"error,omitempty"`
		}{Error: "insufficient capacity for sys.micro"}
		payload, _ := json.Marshal(envelope)
		_ = msg.Respond(payload)
	})
	require.NoError(t, err)
	defer func() { _ = sub.Unsubscribe() }()

	launcher := NewNATSSystemInstanceLauncher(nc, 2*time.Second)
	out, err := launcher.LaunchSystemInstance(&SystemInstanceInput{InstanceType: "sys.micro"})
	require.Error(t, err)
	assert.Nil(t, out)
	assert.Contains(t, err.Error(), "insufficient capacity")
}

func TestNATSSystemInstanceLauncher_LaunchFailsWhenNoResponder(t *testing.T) {
	_, nc, _ := testutil.StartTestJetStream(t)
	defer nc.Close()

	launcher := NewNATSSystemInstanceLauncher(nc, 250*time.Millisecond)
	_, err := launcher.LaunchSystemInstance(&SystemInstanceInput{InstanceType: "sys.micro"})
	require.Error(t, err, "no subscriber on the topic must surface as an error, not hang forever")
	assert.True(t,
		errors.Is(err, nats.ErrNoResponders) || assert.Contains(t, err.Error(), "no responders") || assert.Contains(t, err.Error(), "timeout"),
		"error should surface NATS no-responder / timeout signal: %v", err)
}

func TestNATSSystemInstanceLauncher_TerminateRoundTrip(t *testing.T) {
	_, nc, _ := testutil.StartTestJetStream(t)
	defer nc.Close()

	terminated := make(chan string, 1)
	sub, err := nc.Subscribe("system.TerminateInstance.i-target", func(msg *nats.Msg) {
		terminated <- "i-target"
		payload, _ := json.Marshal(struct {
			Error string `json:"error,omitempty"`
		}{})
		_ = msg.Respond(payload)
	})
	require.NoError(t, err)
	defer func() { _ = sub.Unsubscribe() }()

	launcher := NewNATSSystemInstanceLauncher(nc, 5*time.Second)
	err = launcher.TerminateSystemInstance("i-target")
	require.NoError(t, err)

	select {
	case got := <-terminated:
		assert.Equal(t, "i-target", got)
	case <-time.After(2 * time.Second):
		t.Fatal("terminate subscriber must have been invoked")
	}
}

func TestNewNATSSystemInstanceLauncher_DefaultTimeout(t *testing.T) {
	_, nc, _ := testutil.StartTestJetStream(t)
	defer nc.Close()

	launcher := NewNATSSystemInstanceLauncher(nc, 0)
	concrete, ok := launcher.(*natsSystemInstanceLauncher)
	require.True(t, ok)
	assert.Equal(t, defaultSystemInstanceTimeout, concrete.timeout)

	negative, ok := NewNATSSystemInstanceLauncher(nc, -1*time.Second).(*natsSystemInstanceLauncher)
	require.True(t, ok)
	assert.Equal(t, defaultSystemInstanceTimeout, negative.timeout)
}

func TestNATSSystemInstanceLauncher_LaunchNilInput(t *testing.T) {
	_, nc, _ := testutil.StartTestJetStream(t)
	defer nc.Close()

	launcher := NewNATSSystemInstanceLauncher(nc, time.Second)
	_, err := launcher.LaunchSystemInstance(nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "input is nil")
}

func TestNATSSystemInstanceLauncher_LaunchMissingInstanceType(t *testing.T) {
	_, nc, _ := testutil.StartTestJetStream(t)
	defer nc.Close()

	launcher := NewNATSSystemInstanceLauncher(nc, time.Second)
	_, err := launcher.LaunchSystemInstance(&SystemInstanceInput{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "missing InstanceType")
}

func TestNATSSystemInstanceLauncher_LaunchDecodeFailure(t *testing.T) {
	_, nc, _ := testutil.StartTestJetStream(t)
	defer nc.Close()

	sub, err := nc.Subscribe("system.LaunchInstance.sys.micro", func(msg *nats.Msg) {
		_ = msg.Respond([]byte("{not valid json"))
	})
	require.NoError(t, err)
	defer func() { _ = sub.Unsubscribe() }()

	launcher := NewNATSSystemInstanceLauncher(nc, 2*time.Second)
	_, err = launcher.LaunchSystemInstance(&SystemInstanceInput{InstanceType: "sys.micro"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "decode launch reply")
}

func TestNATSSystemInstanceLauncher_LaunchMissingOutput(t *testing.T) {
	_, nc, _ := testutil.StartTestJetStream(t)
	defer nc.Close()

	sub, err := nc.Subscribe("system.LaunchInstance.sys.micro", func(msg *nats.Msg) {
		_ = msg.Respond([]byte("{}"))
	})
	require.NoError(t, err)
	defer func() { _ = sub.Unsubscribe() }()

	launcher := NewNATSSystemInstanceLauncher(nc, 2*time.Second)
	_, err = launcher.LaunchSystemInstance(&SystemInstanceInput{InstanceType: "sys.micro"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "missing output payload")
}

func TestNATSSystemInstanceLauncher_TerminateEmptyID(t *testing.T) {
	_, nc, _ := testutil.StartTestJetStream(t)
	defer nc.Close()

	launcher := NewNATSSystemInstanceLauncher(nc, time.Second)
	err := launcher.TerminateSystemInstance("")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "empty instance ID")
}

func TestNATSSystemInstanceLauncher_TerminateNoResponder(t *testing.T) {
	_, nc, _ := testutil.StartTestJetStream(t)
	defer nc.Close()

	launcher := NewNATSSystemInstanceLauncher(nc, 250*time.Millisecond)
	err := launcher.TerminateSystemInstance("i-no-one-listens")
	require.Error(t, err)
}

func TestNATSSystemInstanceLauncher_TerminateDecodeFailure(t *testing.T) {
	_, nc, _ := testutil.StartTestJetStream(t)
	defer nc.Close()

	sub, err := nc.Subscribe("system.TerminateInstance.i-decode", func(msg *nats.Msg) {
		_ = msg.Respond([]byte("{not valid json"))
	})
	require.NoError(t, err)
	defer func() { _ = sub.Unsubscribe() }()

	launcher := NewNATSSystemInstanceLauncher(nc, 2*time.Second)
	err = launcher.TerminateSystemInstance("i-decode")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "decode terminate reply")
}

func TestNATSSystemInstanceLauncher_TerminatePropagatesRemoteError(t *testing.T) {
	_, nc, _ := testutil.StartTestJetStream(t)
	defer nc.Close()

	sub, err := nc.Subscribe("system.TerminateInstance.i-missing", func(msg *nats.Msg) {
		payload, _ := json.Marshal(struct {
			Error string `json:"error,omitempty"`
		}{Error: "instance i-missing not found"})
		_ = msg.Respond(payload)
	})
	require.NoError(t, err)
	defer func() { _ = sub.Unsubscribe() }()

	launcher := NewNATSSystemInstanceLauncher(nc, 2*time.Second)
	err = launcher.TerminateSystemInstance("i-missing")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not found")
}
