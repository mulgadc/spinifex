package cmd

import (
	"bytes"
	"encoding/json"
	"errors"
	"log/slog"
	"testing"
	"time"

	"github.com/mulgadc/spinifex/spinifex/config"
	"github.com/mulgadc/spinifex/spinifex/daemon"
	"github.com/mulgadc/spinifex/spinifex/network/external/dhcp"
	"github.com/mulgadc/spinifex/spinifex/testutil"
	"github.com/mulgadc/spinifex/spinifex/types"
	"github.com/nats-io/nats.go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeDrainResponder subscribes a stand-in vpcd to vpc.dhcp.drain that
// replies with the given released count (or error), mirroring
// Manager.handleDrainMsg without a real lease store.
func fakeDrainResponder(t *testing.T, nc *nats.Conn, released int, errMsg string) {
	t.Helper()
	sub, err := nc.Subscribe(dhcp.TopicDrain, func(msg *nats.Msg) {
		body, _ := json.Marshal(struct {
			Released int    `json:"released"`
			Error    string `json:"error,omitempty"`
		}{Released: released, Error: errMsg})
		_ = msg.Respond(body)
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = sub.Unsubscribe() })
}

func TestDrainDHCPLeasesSumsReleased(t *testing.T) {
	_, nc, _ := testutil.StartTestJetStream(t)
	fakeDrainResponder(t, nc, 7, "")

	released, responders := drainDHCPLeases(nc, 2*time.Second)
	assert.Equal(t, 7, released)
	assert.Equal(t, 1, responders)
}

func TestDrainDHCPLeasesReportsResponderError(t *testing.T) {
	_, nc, _ := testutil.StartTestJetStream(t)
	fakeDrainResponder(t, nc, 0, "list leases for drain: boom")

	// An erroring responder still counts as a responder but contributes
	// zero released leases.
	released, responders := drainDHCPLeases(nc, 2*time.Second)
	assert.Equal(t, 0, released)
	assert.Equal(t, 1, responders)
}

func TestDrainDHCPLeasesNoResponders(t *testing.T) {
	_, nc, _ := testutil.StartTestJetStream(t)

	// No vpcd subscribed: the collection window elapses with zero replies.
	released, responders := drainDHCPLeases(nc, 200*time.Millisecond)
	assert.Equal(t, 0, released)
	assert.Equal(t, 0, responders)
}

// TestHostIsStopping covers every systemctl is-system-running outcome the
// shutdown-drain gate must distinguish: a real shutdown/reboot ("stopping"),
// ordinary steady states that must never trigger a drain, unrecognized
// output, and a fully unreachable systemctl.
func TestHostIsStopping(t *testing.T) {
	orig := systemIsSystemRunning
	t.Cleanup(func() { systemIsSystemRunning = orig })

	cases := []struct {
		name         string
		out          string
		err          error
		wantStopping bool
		wantErr      bool
	}{
		{"stopping", "stopping", errors.New("exit status 1"), true, false},
		{"running", "running", nil, false, false},
		{"degraded", "degraded", errors.New("exit status 1"), false, false},
		{"maintenance", "maintenance", errors.New("exit status 1"), false, false},
		{"unknown output", "banana", nil, false, true},
		{"systemctl unreachable", "", errors.New("exec: \"systemctl\": executable file not found in $PATH"), true, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			systemIsSystemRunning = func() (string, error) { return tc.out, tc.err }
			stopping, err := hostIsStopping()
			assert.Equal(t, tc.wantStopping, stopping)
			if tc.wantErr {
				assert.Error(t, err)
			} else {
				assert.NoError(t, err)
			}
		})
	}
}

// TestStackIsRestarting covers every systemctl list-jobs shape the
// stop-vs-restart gate must distinguish: a queued target restart, a direct
// restart of the shutdown unit itself (PartOf propagates target-to-member
// only, so that shows up differently), a plain stop with only stop jobs, no
// pending jobs at all, an unreachable systemctl, and output with no
// parseable job line.
func TestStackIsRestarting(t *testing.T) {
	orig := systemctlListJobs
	t.Cleanup(func() { systemctlListJobs = orig })

	cases := []struct {
		name           string
		out            string
		err            error
		wantRestarting bool
		wantErr        bool
	}{
		{
			name: "target restart queued",
			out: "1655 spinifex-shutdown.service restart running\n" +
				"1654 spinifex-other.service    restart waiting\n" +
				"1636 spinifex.target           start   waiting\n",
			wantRestarting: true,
		},
		{
			name:           "direct restart of shutdown unit",
			out:            "1617 spinifex-shutdown.service restart running\n",
			wantRestarting: true,
		},
		{
			name: "plain stop, only stop jobs",
			out: "1617 spinifex-shutdown.service stop running\n" +
				"1616 spinifex-other.service    stop waiting\n",
			wantRestarting: false,
		},
		{
			name:           "no pending jobs",
			out:            "",
			wantRestarting: false,
		},
		{
			name:           "whitespace-only output",
			out:            "   \n\t\n",
			wantRestarting: false,
		},
		{
			name:    "systemctl unreachable",
			out:     "",
			err:     errors.New("exec: \"systemctl\": executable file not found in $PATH"),
			wantErr: true,
		},
		{
			name:    "unparseable output",
			out:     "totally broken\n",
			wantErr: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			systemctlListJobs = func() (string, error) { return tc.out, tc.err }
			restarting, err := stackIsRestarting()
			assert.Equal(t, tc.wantRestarting, restarting)
			if tc.wantErr {
				assert.Error(t, err)
			} else {
				assert.NoError(t, err)
			}
		})
	}
}

// TestShouldDrainOnStop covers the full stop-vs-restart decision: a real
// shutdown short-circuits the job-list check entirely, a plain stop and both
// restart shapes route correctly, and every "cannot determine" case fails
// toward draining.
func TestShouldDrainOnStop(t *testing.T) {
	origState := systemIsSystemRunning
	origJobs := systemctlListJobs
	t.Cleanup(func() {
		systemIsSystemRunning = origState
		systemctlListJobs = origJobs
	})

	t.Run("real shutdown short-circuits list-jobs", func(t *testing.T) {
		systemIsSystemRunning = func() (string, error) { return "stopping", errors.New("exit status 1") }
		systemctlListJobs = func() (string, error) {
			t.Fatal("stackIsRestarting must not run once hostIsStopping is true")
			return "", nil
		}
		drain, reason := shouldDrainOnStop()
		assert.True(t, drain)
		assert.NotEmpty(t, reason)
	})

	cases := []struct {
		name      string
		state     string
		stateErr  error
		jobs      string
		jobsErr   error
		wantDrain bool
	}{
		{
			name:      "plain stop, no restart queued",
			state:     "running",
			jobs:      "1617 spinifex-shutdown.service stop running\n",
			wantDrain: true,
		},
		{
			name:      "target restart queued",
			state:     "running",
			jobs:      "1636 spinifex.target start waiting\n",
			wantDrain: false,
		},
		{
			name:      "direct restart of shutdown unit",
			state:     "running",
			jobs:      "1617 spinifex-shutdown.service restart running\n",
			wantDrain: false,
		},
		{
			name:      "unknown host state",
			state:     "banana",
			wantDrain: true,
		},
		{
			name:      "is-system-running unreachable",
			state:     "",
			stateErr:  errors.New("exec: \"systemctl\": executable file not found in $PATH"),
			wantDrain: true,
		},
		{
			name:      "list-jobs unreachable",
			state:     "running",
			jobsErr:   errors.New("exec: \"systemctl\": executable file not found in $PATH"),
			wantDrain: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			systemIsSystemRunning = func() (string, error) { return tc.state, tc.stateErr }
			systemctlListJobs = func() (string, error) { return tc.jobs, tc.jobsErr }
			drain, reason := shouldDrainOnStop()
			assert.Equal(t, tc.wantDrain, drain)
			assert.NotEmpty(t, reason)
		})
	}
}

// TestNodeDrainCmdDeprecatedFlagAlias asserts --only-if-host-stopping still
// exists and works (a node running the new unit file against an old spx
// binary, or the reverse during a partial upgrade, must not break) but is
// marked deprecated in favour of --unless-restarting.
func TestNodeDrainCmdDeprecatedFlagAlias(t *testing.T) {
	unlessRestarting := nodeDrainCmd.Flags().Lookup("unless-restarting")
	require.NotNil(t, unlessRestarting)
	assert.Empty(t, unlessRestarting.Deprecated)

	legacy := nodeDrainCmd.Flags().Lookup("only-if-host-stopping")
	require.NotNil(t, legacy)
	assert.NotEmpty(t, legacy.Deprecated)
}

// captureSlog swaps the default slog logger for a text handler writing into
// a buffer for the duration of fn, then restores it.
func captureSlog(t *testing.T, fn func()) string {
	t.Helper()
	var buf bytes.Buffer
	orig := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(orig) })

	fn()

	return buf.String()
}

// TestWarnIfGuestsLeftRunningNamesRunningGuests asserts the skip branch
// enumerates via the same spinifex.node.vms fan-out spx get vms uses, warns
// at WARN level, and names only the running instance, not the stopped one.
func TestWarnIfGuestsLeftRunningNamesRunningGuests(t *testing.T) {
	origTimeout := guestEnumerationTimeout
	guestEnumerationTimeout = 200 * time.Millisecond
	t.Cleanup(func() { guestEnumerationTimeout = origTimeout })

	_, nc, _ := testutil.StartTestJetStream(t)
	stubConnect(t, &config.ClusterConfig{Node: "node1"}, nc, nil)

	sub, err := nc.Subscribe("spinifex.node.vms", func(msg *nats.Msg) {
		resp := types.NodeVMsResponse{
			Node: "node1",
			VMs: []types.VMInfo{
				{InstanceID: "i-running", Status: vmStatusRunning},
				{InstanceID: "i-stopped", Status: "stopped"},
			},
		}
		body, _ := json.Marshal(resp)
		_ = msg.Respond(body)
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = sub.Unsubscribe() })

	out := captureSlog(t, warnIfGuestsLeftRunning)

	assert.Contains(t, out, "i-running")
	assert.NotContains(t, out, "i-stopped")
	assert.Contains(t, out, "WARN")
}

// TestWarnIfGuestsLeftRunningNoGuestsIsSilent asserts a node with no
// running guests produces no warning.
func TestWarnIfGuestsLeftRunningNoGuestsIsSilent(t *testing.T) {
	origTimeout := guestEnumerationTimeout
	guestEnumerationTimeout = 200 * time.Millisecond
	t.Cleanup(func() { guestEnumerationTimeout = origTimeout })

	_, nc, _ := testutil.StartTestJetStream(t)
	stubConnect(t, &config.ClusterConfig{Node: "node1"}, nc, nil)

	sub, err := nc.Subscribe("spinifex.node.vms", func(msg *nats.Msg) {
		resp := types.NodeVMsResponse{
			Node: "node1",
			VMs:  []types.VMInfo{{InstanceID: "i-stopped", Status: "stopped"}},
		}
		body, _ := json.Marshal(resp)
		_ = msg.Respond(body)
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = sub.Unsubscribe() })

	out := captureSlog(t, warnIfGuestsLeftRunning)
	assert.Empty(t, out)
}

// TestWarnIfGuestsLeftRunningConnectFailureWarnsNotFails asserts that a
// failure to even connect for enumeration is itself warned about rather
// than treated as fatal — a skipped drain must never block the stop.
func TestWarnIfGuestsLeftRunningConnectFailureWarnsNotFails(t *testing.T) {
	stubConnect(t, nil, nil, errors.New("connect refused"))

	out := captureSlog(t, warnIfGuestsLeftRunning)
	assert.Contains(t, out, "could not connect")
}

// stubCollectShutdownACKsFn swaps collectShutdownACKsFn for fn and restores
// the original on cleanup, so collectLocalShutdownACK's retry loop can be
// driven deterministically without a real NATS round trip.
func stubCollectShutdownACKsFn(t *testing.T, fn func(nc *nats.Conn, topic string, reqData []byte, nodeCount int, nodeFilter string, timeout time.Duration) ([]daemon.ShutdownACK, error)) {
	t.Helper()
	orig := collectShutdownACKsFn
	t.Cleanup(func() { collectShutdownACKsFn = orig })
	collectShutdownACKsFn = fn
}

// shrinkLocalShutdownACKBackoff overrides the GATE/DRAIN retry backoff to
// millisecond-scale values for the duration of the test so retry tests do
// not pay for the real 250ms-2s production backoff.
func shrinkLocalShutdownACKBackoff(t *testing.T) {
	t.Helper()
	origBase, origCap := localShutdownACKRetryBaseDelay, localShutdownACKRetryMaxDelay
	localShutdownACKRetryBaseDelay = time.Millisecond
	localShutdownACKRetryMaxDelay = 5 * time.Millisecond
	t.Cleanup(func() {
		localShutdownACKRetryBaseDelay = origBase
		localShutdownACKRetryMaxDelay = origCap
	})
}

// TestLocalDrainRequestIsNodeScoped pins the field that keeps a local drain
// local. The phase subjects are fan-out, so an empty Target here drains the
// guests on every node in the cluster, not just the one being stopped.
func TestLocalDrainRequestIsNodeScoped(t *testing.T) {
	req := localDrainRequest("drain", "node2", 90*time.Second)

	assert.Equal(t, "node2", req.Target, "a local drain must name the node it applies to")
	assert.Equal(t, "drain", req.Phase)
	assert.Equal(t, 90, req.Timeout)
}

// TestSummariseShutdownACKs covers phase accounting by outcome: a node that
// answered with an error has not completed the phase, and counting it as done
// advances the shutdown into STORAGE with guests still running.
func TestSummariseShutdownACKs(t *testing.T) {
	tests := []struct {
		name      string
		acks      []daemon.ShutdownACK
		completed int
		failed    []string
	}{
		{
			name:      "all clean",
			acks:      []daemon.ShutdownACK{{Node: "node1"}, {Node: "node2"}},
			completed: 2,
		},
		{
			name: "one errored is not counted as completed",
			acks: []daemon.ShutdownACK{
				{Node: "node1"},
				{Node: "node2", Error: "failed to stop VMs"},
				{Node: "node3"},
			},
			completed: 2,
			failed:    []string{"node2"},
		},
		{
			name:      "all errored",
			acks:      []daemon.ShutdownACK{{Node: "node1", Error: "boom"}, {Node: "node2", Error: "boom"}},
			completed: 0,
			failed:    []string{"node1", "node2"},
		},
		{
			name:      "no acks",
			acks:      nil,
			completed: 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			completed, failed := summariseShutdownACKs(tt.acks)
			assert.Equal(t, tt.completed, completed)
			assert.Equal(t, tt.failed, failed)
		})
	}
}

// TestShutdownPhaseOutcome covers the gate between phases. Advancing past a
// node that failed to drain is what tears storage out from under a guest that
// never stopped, so only --force may do it.
func TestShutdownPhaseOutcome(t *testing.T) {
	clean := []daemon.ShutdownACK{{Node: "node1"}, {Node: "node2"}, {Node: "node3"}}
	mixed := []daemon.ShutdownACK{{Node: "node1"}, {Node: "node2", Error: "failed to stop VMs"}, {Node: "node3"}}

	t.Run("every node clean advances", func(t *testing.T) {
		completed, abort := shutdownPhaseOutcome("drain", clean, 3, false)
		assert.Equal(t, 3, completed)
		assert.Empty(t, abort)
	})

	t.Run("an errored node aborts and is named", func(t *testing.T) {
		completed, abort := shutdownPhaseOutcome("drain", mixed, 3, false)
		assert.Equal(t, 2, completed)
		assert.Contains(t, abort, "node2")
		assert.Contains(t, abort, "failed the phase")
		assert.Contains(t, abort, "--force")
	})

	t.Run("a silent node aborts on the count", func(t *testing.T) {
		completed, abort := shutdownPhaseOutcome("storage", clean, 5, false)
		assert.Equal(t, 3, completed)
		assert.Contains(t, abort, "3/5")
	})

	t.Run("force advances past both", func(t *testing.T) {
		completed, abort := shutdownPhaseOutcome("drain", mixed, 5, true)
		assert.Equal(t, 2, completed)
		assert.Empty(t, abort, "--force is the documented way to continue despite errors")
	})
}

// TestCollectLocalShutdownACKRetriesUntilACK covers the actual bug: the
// daemon may not have re-subscribed right after a restart, so the first
// attempt(s) get no ACK. The ACK on the third attempt must still succeed.
func TestCollectLocalShutdownACKRetriesUntilACK(t *testing.T) {
	shrinkLocalShutdownACKBackoff(t)

	var calls int
	stubCollectShutdownACKsFn(t, func(nc *nats.Conn, topic string, reqData []byte, nodeCount int, nodeFilter string, timeout time.Duration) ([]daemon.ShutdownACK, error) {
		calls++
		if calls < 3 {
			return nil, nil // daemon not re-subscribed yet: no ACK, no error
		}
		return []daemon.ShutdownACK{{Node: nodeFilter, Phase: "gate"}}, nil
	})

	ack, err := collectLocalShutdownACK(nil, "spinifex.cluster.shutdown.gate", []byte(`{}`), "node1", time.Second)
	require.NoError(t, err)
	assert.Equal(t, "node1", ack.Node)
	assert.Equal(t, 3, calls)
}

// TestCollectLocalShutdownACKExhaustedReturnsWithinBudget covers a daemon
// that never re-subscribes: the call must return promptly once the budget
// elapses, not hang, with a non-nil error so the failure path fires.
func TestCollectLocalShutdownACKExhaustedReturnsWithinBudget(t *testing.T) {
	shrinkLocalShutdownACKBackoff(t)

	var calls int
	stubCollectShutdownACKsFn(t, func(nc *nats.Conn, topic string, reqData []byte, nodeCount int, nodeFilter string, timeout time.Duration) ([]daemon.ShutdownACK, error) {
		calls++
		return nil, nil // daemon never comes back within the budget
	})

	budget := 60 * time.Millisecond
	start := time.Now()
	ack, err := collectLocalShutdownACK(nil, "spinifex.cluster.shutdown.gate", []byte(`{}`), "node1", budget)
	elapsed := time.Since(start)

	require.Error(t, err)
	assert.Equal(t, daemon.ShutdownACK{}, ack)
	assert.Contains(t, err.Error(), "node1")
	assert.Greater(t, calls, 1, "must have retried, not failed on the first miss")
	// The retry loop must not run appreciably past the configured budget.
	assert.Less(t, elapsed, budget+150*time.Millisecond)
}

// TestReportGuestsLeftRunningSevereLogsErrorAndNamesGuests covers the
// "drain failed" path sharing warnIfGuestsLeftRunning's enumeration: severe
// must log at ERROR, not WARN, and still name only the running instance.
func TestReportGuestsLeftRunningSevereLogsErrorAndNamesGuests(t *testing.T) {
	origTimeout := guestEnumerationTimeout
	guestEnumerationTimeout = 200 * time.Millisecond
	t.Cleanup(func() { guestEnumerationTimeout = origTimeout })

	_, nc, _ := testutil.StartTestJetStream(t)
	stubConnect(t, &config.ClusterConfig{Node: "node1"}, nc, nil)

	sub, err := nc.Subscribe("spinifex.node.vms", func(msg *nats.Msg) {
		resp := types.NodeVMsResponse{
			Node: "node1",
			VMs: []types.VMInfo{
				{InstanceID: "i-abandoned", Status: vmStatusRunning},
				{InstanceID: "i-stopped", Status: "stopped"},
			},
		}
		body, _ := json.Marshal(resp)
		_ = msg.Respond(body)
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = sub.Unsubscribe() })

	out := captureSlog(t, func() {
		reportGuestsLeftRunning(true, "drain failed: guests left running with storage about to be torn down")
	})

	assert.Contains(t, out, "ERROR")
	assert.Contains(t, out, "i-abandoned")
	assert.NotContains(t, out, "i-stopped")
}
