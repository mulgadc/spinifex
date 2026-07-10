package admin

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestReadMachineID_ReturnsNonEmpty(t *testing.T) {
	t.Parallel()
	id := ReadMachineID()
	assert.NotEmpty(t, id, "machine ID should never be empty")
	assert.Greater(t, len(id), 8, "machine ID should be a reasonable length")
}

func TestSendTelemetry_PostsCorrectPayload(t *testing.T) {
	t.Parallel()
	var received TelemetryPayload

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, http.MethodPost, r.Method)
		assert.Equal(t, "application/json", r.Header.Get("Content-Type"))

		body, err := io.ReadAll(r.Body)
		require.NoError(t, err)
		require.NoError(t, json.Unmarshal(body, &received))

		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	ctx := context.Background()
	SendTelemetry(ctx, TelemetryPayload{
		MachineID: "test-machine-123",
		Event:     "init",
		Region:    "ap-southeast-2",
		AZ:        "ap-southeast-2a",
		Node:      "node1",
		Nodes:     3,
		BindIP:    "10.11.12.1",
		Version:   "v0.5.0",
		URL:       server.URL,
	})

	assert.Equal(t, "test-machine-123", received.MachineID)
	assert.Equal(t, "init", received.Event)
	assert.Equal(t, "ap-southeast-2", received.Region)
	assert.Equal(t, 3, received.Nodes)
	assert.Equal(t, "v0.5.0", received.Version)
	assert.NotEmpty(t, received.Arch, "arch should be auto-filled")
	assert.NotEmpty(t, received.OS, "os should be auto-filled")
	assert.NotEmpty(t, received.Timestamp, "timestamp should be auto-filled")
}

func TestSendTelemetry_RespectsTimeout(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Outlive the 100ms client deadline so SendTelemetry aborts on its own
		// context, but return quickly enough that server.Close() does not block.
		time.Sleep(300 * time.Millisecond)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	start := time.Now()
	SendTelemetry(ctx, TelemetryPayload{MachineID: "timeout-test", URL: server.URL})
	elapsed := time.Since(start)

	assert.Less(t, elapsed, 2*time.Second, "should respect context timeout")
}

func TestSendTelemetry_HandlesServerError(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()

	ctx := context.Background()
	SendTelemetry(ctx, TelemetryPayload{MachineID: "error-test", URL: server.URL})
}

func TestSendTelemetry_HandlesUnreachableServer(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	// URL pointing to a port that's not listening
	SendTelemetry(ctx, TelemetryPayload{MachineID: "unreachable-test", URL: "http://127.0.0.1:1"})
}
