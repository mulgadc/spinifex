package admin

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
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
		if !assert.NoError(t, err) {
			http.Error(w, "read request body", http.StatusBadRequest)
			return
		}
		if !assert.NoError(t, json.Unmarshal(body, &received)) {
			http.Error(w, "decode request body", http.StatusBadRequest)
			return
		}

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

func TestReadCampaign_FromFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "campaign")

	assert.Empty(t, readCampaignFrom(path), "a missing file means an unattributed install")

	for _, tc := range []struct {
		name    string
		content string
		want    string
	}{
		{"code", "rd-eks-baremetal\n", "rd-eks-baremetal"},
		{"untrimmed", "  dt-egress-math  \n", "dt-egress-math"},
		{"unmapped prefix is still a code", "eks-baremetal\n", "eks-baremetal"},
		{"uppercase", "RD-eks\n", ""},
		{"no prefix", "ekseksbaremetal\n", ""},
		{"shell metacharacters", "rd-eks; rm -rf /\n", ""},
		{"empty", "\n", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert.NoError(t, os.WriteFile(path, []byte(tc.content), 0o600))
			assert.Equal(t, tc.want, readCampaignFrom(path))
		})
	}
}

func TestReadCampaign_EnvOverridesFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "campaign")
	assert.NoError(t, os.WriteFile(path, []byte("rd-eks-baremetal\n"), 0o600))

	t.Setenv("SPINIFEX_CAMPAIGN", "hn-cost-writeup")
	assert.Equal(t, "hn-cost-writeup", readCampaignFrom(path))

	t.Setenv("SPINIFEX_CAMPAIGN", "not a code")
	assert.Empty(t, readCampaignFrom(path), "a malformed override is not silently replaced by the file")
}

func TestSendTelemetry_FillsCampaign(t *testing.T) {
	var received TelemetryPayload
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		assert.NoError(t, err)
		assert.NoError(t, json.Unmarshal(body, &received))
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	t.Setenv("SPINIFEX_CAMPAIGN", "rd-eks-baremetal")
	SendTelemetry(context.Background(), TelemetryPayload{MachineID: "campaign-test", Event: "init", URL: server.URL})

	assert.Equal(t, "rd-eks-baremetal", received.Campaign)
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
