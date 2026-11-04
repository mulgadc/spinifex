//go:build lambdatestlink

package agent_test

import (
	"context"
	"net"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mulgadc/spinifex/spinifex/agents/lambda/agent"
	"github.com/mulgadc/spinifex/spinifex/agents/lambda/hostlink"
	"github.com/mulgadc/spinifex/spinifex/agents/lambda/runtimeapi"
)

// An expired invocation's timeout result is written over a real connection
// after the deadline, within the grace, and the host decodes it.
func TestRunWritesTheTimeoutFrameAfterTheDeadline(t *testing.T) {
	guestConn, hostConn := net.Pipe()
	t.Cleanup(func() {
		_ = guestConn.Close()
		_ = hostConn.Close()
	})
	guest := hostlink.NewTestStream(guestConn)
	host := hostlink.NewTestStream(hostConn)

	dl := time.Now().Add(50 * time.Millisecond)
	req := hostlink.Request{GrantID: "g", AttemptID: "a", RequestID: "r", DeadlineMs: dl.UnixMilli()}
	sent := make(chan error, 1)
	go func() { sent <- host.HostSend(req) }()

	done := make(chan error, 1)
	go func() { done <- agent.Run(t.Context(), guest, runtimeapi.New()) }()
	select {
	case err := <-sent:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("the request was not delivered")
	}

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	resp, err := host.HostResult(ctx)
	got := time.Now()
	require.NoError(t, err)
	assert.Equal(t, "Sandbox.Timedout", resp.FunctionError)
	assert.Equal(t, "r", resp.RequestID)
	assert.Equal(t, "g", resp.GrantID)
	assert.Equal(t, "a", resp.AttemptID)
	assert.False(t, got.Before(time.UnixMilli(req.DeadlineMs)), "the result was read before the deadline")

	select {
	case err := <-done:
		require.ErrorIs(t, err, agent.ErrInvocationTimedOut)
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after writing the timeout result")
	}
}
