package host

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mulgadc/spinifex/spinifex/config"
	"github.com/mulgadc/spinifex/spinifex/utils"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func multiNodeClusterConfig() *config.ClusterConfig {
	return &config.ClusterConfig{
		Node: "node1",
		Nodes: map[string]config.Config{
			"node1": {},
			"node2": {},
		},
	}
}

type recordingSudo struct {
	runs          [][]string
	activeOutput  string
	enabledOutput map[string]string
	activePerUnit map[string]string
	// nbUnreachable stands in for a node whose ovn-central is not answering, or
	// which is not a management node at all. The socket file exists in both
	// cases, which is why its presence was never the right question.
	nbUnreachable bool
	// nbIPSec is what NB_Global.ipsec currently reads as.
	nbIPSec string
}

func (r *recordingSudo) stub(name string, args ...string) *exec.Cmd {
	r.runs = append(r.runs, append([]string{name}, args...))
	if name == "ovn-nbctl" {
		if r.nbUnreachable {
			return exec.Command("sh", "-c",
				`echo "ovn-nbctl: unix:/var/run/ovn/ovnnb_db.sock: database connection failed" >&2; exit 1`)
		}
		if len(args) > 1 && args[1] == "get" {
			return exec.Command("printf", "%s\n", r.nbIPSec)
		}
		return exec.Command("true")
	}
	if name != "systemctl" || len(args) < 2 {
		return exec.Command("true")
	}
	unit := args[len(args)-1]
	switch args[0] {
	case "is-active":
		if out, ok := r.activePerUnit[unit]; ok {
			return exec.Command("printf", "%s", out)
		}
		out := r.activeOutput
		if out == "" {
			out = "active\n"
		}
		return exec.Command("printf", "%s", out)
	case "is-enabled":
		if out, ok := r.enabledOutput[unit]; ok {
			return exec.Command("printf", "%s", out)
		}
		return exec.Command("printf", "%s", "enabled\n")
	}
	return exec.Command("true")
}

// helperRuns returns the IPsec state-helper invocations, dropping the
// is-enabled/is-active probes that precede them.
func (r *recordingSudo) helperRuns() [][]string {
	var out [][]string
	for _, run := range r.runs {
		if run[0] == ipsecStateHelper {
			out = append(out, run)
		}
	}
	return out
}

func multiNodeIPSecConfig(enabled bool) *config.ClusterConfig {
	cfg := multiNodeClusterConfig()
	cfg.Network.IPSecEnabled = enabled
	return cfg
}

// fakeBarrier stands in for the cluster readiness channel.
type fakeBarrier struct {
	published     []bool
	publishedNode string
	publishErr    error

	ready    bool
	pending  []string
	readyErr error
}

func (f *fakeBarrier) PublishLocalReady(_ context.Context, node string, ready bool) error {
	f.publishedNode = node
	f.published = append(f.published, ready)
	return f.publishErr
}

func (f *fakeBarrier) NodesReady(_ context.Context, _ []string) (bool, []string, error) {
	return f.ready, f.pending, f.readyErr
}

// allReady is the steady state: every chassis has finished its local setup.
func allReady() *fakeBarrier { return &fakeBarrier{ready: true} }

// ipsecTestConfigDir lays out the credentials EnableOVNIPSec insists on.
func ipsecTestConfigDir(t *testing.T) string {
	t.Helper()
	configDir := t.TempDir()
	configPath := filepath.Join(configDir, "spinifex.toml")
	require.NoError(t, os.WriteFile(configPath, []byte("placeholder"), 0600))
	for _, rel := range []string{"ca.pem", "ipsec/peer.pem", "ipsec/peer.key"} {
		full := filepath.Join(configDir, rel)
		require.NoError(t, os.MkdirAll(filepath.Dir(full), 0750))
		require.NoError(t, os.WriteFile(full, []byte("x"), 0600))
	}
	return configDir
}

// nbctlWrites returns the ovn-nbctl invocations that write, dropping the reads.
func (r *recordingSudo) nbctlWrites() [][]string {
	var out [][]string
	for _, run := range r.runs {
		if run[0] == "ovn-nbctl" && len(run) > 2 && run[2] == "set" {
			out = append(out, run)
		}
	}
	return out
}

func TestEnableOVNIPSec(t *testing.T) {
	recorder := &recordingSudo{nbUnreachable: true}
	t.Cleanup(utils.SetSudoCommandForTest(recorder.stub))

	configDir := ipsecTestConfigDir(t)
	configPath := filepath.Join(configDir, "spinifex.toml")

	require.NoError(t, EnableOVNIPSec(t.Context(), configPath, multiNodeClusterConfig(), allReady()))

	require.Len(t, recorder.runs, 4)
	assert.Equal(t, []string{"systemctl", "is-active", "openvswitch-ipsec.service"}, recorder.runs[0])
	for _, run := range recorder.runs[1:3] {
		assert.Equal(t, "ovs-vsctl", run[0])
		assert.Equal(t, "set", run[1])
		assert.Equal(t, "Open_vSwitch", run[2])
	}
	joined := strings.Join(recorder.runs[1], " ")
	assert.Contains(t, joined, "other_config:certificate="+filepath.Join(configDir, "ipsec", "peer.pem"))
	assert.Contains(t, joined, "other_config:private_key="+filepath.Join(configDir, "ipsec", "peer.key"))
	assert.Contains(t, joined, "other_config:ca_cert="+filepath.Join(configDir, "ca.pem"))
	assert.Contains(t, strings.Join(recorder.runs[2], " "), "other_config:ipsec_encapsulation=true")

	// The NB DB is unreachable, so the read is attempted and nothing is written.
	assert.Empty(t, recorder.nbctlWrites())
}

func TestEnableOVNIPSec_Management(t *testing.T) {
	recorder := &recordingSudo{}
	t.Cleanup(utils.SetSudoCommandForTest(recorder.stub))

	configPath := filepath.Join(ipsecTestConfigDir(t), "spinifex.toml")

	require.NoError(t, EnableOVNIPSec(t.Context(), configPath, multiNodeClusterConfig(), allReady()))

	assert.Equal(t, [][]string{{"ovn-nbctl", "--timeout=5", "set", "NB_Global", ".", "ipsec=true"}},
		recorder.nbctlWrites())
}

// The reported incident: one node asserts encryption cluster-wide while the
// others have not finished, and every guest crossing chassis black-holes.
func TestEnableOVNIPSec_HoldsFlagUntilEveryChassisIsReady(t *testing.T) {
	recorder := &recordingSudo{}
	t.Cleanup(utils.SetSudoCommandForTest(recorder.stub))

	configPath := filepath.Join(ipsecTestConfigDir(t), "spinifex.toml")
	barrier := &fakeBarrier{pending: []string{"node2"}}

	require.NoError(t, EnableOVNIPSec(t.Context(), configPath, multiNodeClusterConfig(), barrier))

	assert.Equal(t, "node1", barrier.publishedNode)
	assert.Equal(t, []bool{true}, barrier.published,
		"the local half completed, so this node must report itself ready")
	assert.Empty(t, recorder.nbctlWrites(),
		"NB_Global must not be asserted while a chassis is unconfigured")
}

// A flag left asserted over an unconfigured chassis drops guest traffic on the
// floor; plaintext is the state the cluster had before IPsec was asked for.
func TestEnableOVNIPSec_RetractsFlagWhenAChassisRegresses(t *testing.T) {
	recorder := &recordingSudo{nbIPSec: "true"}
	t.Cleanup(utils.SetSudoCommandForTest(recorder.stub))

	configPath := filepath.Join(ipsecTestConfigDir(t), "spinifex.toml")

	require.NoError(t, EnableOVNIPSec(t.Context(), configPath, multiNodeClusterConfig(),
		&fakeBarrier{pending: []string{"node2"}}))

	assert.Equal(t, [][]string{{"ovn-nbctl", "--timeout=5", "set", "NB_Global", ".", "ipsec=false"}},
		recorder.nbctlWrites())
}

// An unreadable barrier is not evidence that a chassis is unconfigured, so it
// must never downgrade a working encrypted mesh to plaintext.
func TestEnableOVNIPSec_BarrierErrorLeavesTheFlagAlone(t *testing.T) {
	recorder := &recordingSudo{nbIPSec: "true"}
	t.Cleanup(utils.SetSudoCommandForTest(recorder.stub))

	configPath := filepath.Join(ipsecTestConfigDir(t), "spinifex.toml")

	err := EnableOVNIPSec(t.Context(), configPath, multiNodeClusterConfig(),
		&fakeBarrier{readyErr: errors.New("kv unavailable")})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "read IPsec readiness")
	assert.Empty(t, recorder.nbctlWrites())
}

// Already true and everyone ready: no write, so a steady-state pass is free.
func TestEnableOVNIPSec_AlreadyAssertedIsANoOp(t *testing.T) {
	recorder := &recordingSudo{nbIPSec: "true"}
	t.Cleanup(utils.SetSudoCommandForTest(recorder.stub))

	configPath := filepath.Join(ipsecTestConfigDir(t), "spinifex.toml")

	require.NoError(t, EnableOVNIPSec(t.Context(), configPath, multiNodeClusterConfig(), allReady()))
	assert.Empty(t, recorder.nbctlWrites())
}

// A node that cannot publish must fail its pass rather than proceed: silently
// dropping the record would make it pending forever to everyone else.
func TestEnableOVNIPSec_PublishFailureFailsThePass(t *testing.T) {
	recorder := &recordingSudo{}
	t.Cleanup(utils.SetSudoCommandForTest(recorder.stub))

	configPath := filepath.Join(ipsecTestConfigDir(t), "spinifex.toml")

	err := EnableOVNIPSec(t.Context(), configPath, multiNodeClusterConfig(),
		&fakeBarrier{ready: true, publishErr: errors.New("kv unavailable")})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "publish IPsec readiness")
	assert.Empty(t, recorder.nbctlWrites())
}

func TestEnableOVNIPSec_SingleNodeSkip(t *testing.T) {
	t.Cleanup(utils.SetSudoCommandForTest(func(name string, args ...string) *exec.Cmd {
		t.Fatalf("utils.SudoCommand must not run on single-node short-circuit; got %s %v", name, args)
		return exec.Command("true")
	}))

	configDir := t.TempDir()
	configPath := filepath.Join(configDir, "spinifex.toml")
	require.NoError(t, os.WriteFile(configPath, []byte("placeholder"), 0600))

	cfg := &config.ClusterConfig{
		Node:  "node1",
		Nodes: map[string]config.Config{"node1": {}},
	}
	require.NoError(t, EnableOVNIPSec(t.Context(), configPath, cfg, allReady()))
}

func TestEnableOVNIPSec_MonitorIPSecInactive(t *testing.T) {
	recorder := &recordingSudo{activeOutput: "inactive\n"}
	t.Cleanup(utils.SetSudoCommandForTest(recorder.stub))

	origTimeout := systemctlActiveTimeout
	systemctlActiveTimeout = 100 * time.Millisecond
	t.Cleanup(func() { systemctlActiveTimeout = origTimeout })

	configDir := t.TempDir()
	configPath := filepath.Join(configDir, "spinifex.toml")
	require.NoError(t, os.WriteFile(configPath, []byte("placeholder"), 0600))
	for _, rel := range []string{"ca.pem", "ipsec/peer.pem", "ipsec/peer.key"} {
		full := filepath.Join(configDir, rel)
		require.NoError(t, os.MkdirAll(filepath.Dir(full), 0750))
		require.NoError(t, os.WriteFile(full, []byte("x"), 0600))
	}

	err := EnableOVNIPSec(t.Context(), configPath, multiNodeClusterConfig(), allReady())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "ovs-monitor-ipsec")
	assert.Contains(t, err.Error(), "not active")

	// ovs-vsctl must NOT run — flip without live daemon is the silent-drop trap.
	for _, run := range recorder.runs {
		assert.NotEqual(t, "ovs-vsctl", run[0], "ovs-vsctl invoked despite dead daemon: %v", run)
	}
}

func TestEnableOVNIPSec_MissingCert(t *testing.T) {
	t.Cleanup(utils.SetSudoCommandForTest(func(name string, args ...string) *exec.Cmd {
		t.Fatalf("utils.SudoCommand must not run when cert files are absent")
		return exec.Command("true")
	}))

	configDir := t.TempDir()
	configPath := filepath.Join(configDir, "spinifex.toml")
	require.NoError(t, os.WriteFile(configPath, []byte("placeholder"), 0600))
	require.NoError(t, os.WriteFile(filepath.Join(configDir, "ca.pem"), []byte("x"), 0600))

	err := EnableOVNIPSec(t.Context(), configPath, multiNodeClusterConfig(), allReady())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "missing IPsec credential")
}

func TestEnableOVNIPSec_NoConfigPath(t *testing.T) {
	err := EnableOVNIPSec(t.Context(), "", nil, nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "config path unset")
}

func TestReconcileOVNIPSec_DisabledStopsCharon(t *testing.T) {
	recorder := &recordingSudo{}
	t.Cleanup(utils.SetSudoCommandForTest(recorder.stub))

	require.NoError(t, ReconcileOVNIPSec(t.Context(), "/etc/spinifex/spinifex.toml", multiNodeIPSecConfig(false), allReady()))

	assert.Equal(t, [][]string{{ipsecStateHelper, "off"}}, recorder.helperRuns())

	for _, run := range recorder.runs {
		assert.NotEqual(t, "ovs-vsctl", run[0], "must not touch OVS when IPsec is off: %v", run)
	}
}

// The daemon has no systemctl grant, so a unit change must never be attempted
// directly: it would fail at the polkit prompt and leave charon listening.
func TestReconcileOVNIPSec_NeverCallsSystemctlDirectly(t *testing.T) {
	recorder := &recordingSudo{}
	t.Cleanup(utils.SetSudoCommandForTest(recorder.stub))

	require.NoError(t, ReconcileOVNIPSec(t.Context(), "/etc/spinifex/spinifex.toml", multiNodeIPSecConfig(false), allReady()))

	for _, run := range recorder.runs {
		if run[0] != "systemctl" {
			continue
		}
		assert.Contains(t, []string{"is-active", "is-enabled"}, run[1],
			"only read-only systemctl verbs may be run directly: %v", run)
	}
}

// A single-node cluster has no tunnels to protect, so charon must not be left
// listening even though ipsec_enabled defaults to true.
func TestReconcileOVNIPSec_SingleNodeStopsCharon(t *testing.T) {
	recorder := &recordingSudo{}
	t.Cleanup(utils.SetSudoCommandForTest(recorder.stub))

	cfg := &config.ClusterConfig{Node: "node1", Nodes: map[string]config.Config{"node1": {}}}
	cfg.Network.IPSecEnabled = true

	require.NoError(t, ReconcileOVNIPSec(t.Context(), "/etc/spinifex/spinifex.toml", cfg, allReady()))

	assert.Equal(t, [][]string{{ipsecStateHelper, "off"}}, recorder.helperRuns())
}

func TestReconcileOVNIPSec_EnabledTurnsTheServicesOn(t *testing.T) {
	recorder := &recordingSudo{
		enabledOutput: map[string]string{
			ovsIPSecUnit:          "disabled\n",
			strongswanStarterUnit: "disabled\n",
		},
		activePerUnit: map[string]string{
			ovsIPSecUnit:          "active\n",
			strongswanStarterUnit: "inactive\n",
		},
	}
	t.Cleanup(utils.SetSudoCommandForTest(recorder.stub))

	recorder.nbUnreachable = true
	configPath := filepath.Join(ipsecTestConfigDir(t), "spinifex.toml")

	require.NoError(t, ReconcileOVNIPSec(t.Context(), configPath, multiNodeIPSecConfig(true), allReady()))

	assert.Equal(t, [][]string{{ipsecStateHelper, "on"}}, recorder.helperRuns())

	// The enable path must still reach the OVS writes.
	var sawEncapsulation bool
	for _, run := range recorder.runs {
		if run[0] == "ovs-vsctl" && strings.Contains(strings.Join(run, " "), "ipsec_encapsulation=true") {
			sawEncapsulation = true
		}
	}
	assert.True(t, sawEncapsulation, "ipsec_encapsulation was never set: %v", recorder.runs)
}

func TestReconcileOVNIPSec_AlreadyInStateIsNoOp(t *testing.T) {
	recorder := &recordingSudo{
		enabledOutput: map[string]string{strongswanStarterUnit: "disabled\n", ovsIPSecUnit: "disabled\n"},
		activePerUnit: map[string]string{strongswanStarterUnit: "inactive\n", ovsIPSecUnit: "inactive\n"},
	}
	t.Cleanup(utils.SetSudoCommandForTest(recorder.stub))

	require.NoError(t, ReconcileOVNIPSec(t.Context(), "/etc/spinifex/spinifex.toml", multiNodeIPSecConfig(false), allReady()))
	assert.Empty(t, recorder.helperRuns())
}

func TestReconcileOVNIPSec_UnknownUnitIsNotAnError(t *testing.T) {
	recorder := &recordingSudo{
		enabledOutput: map[string]string{
			strongswanStarterUnit: "Failed to get unit file state for strongswan-starter.service: No such file or directory\n",
			ovsIPSecUnit:          "Failed to get unit file state for openvswitch-ipsec.service: No such file or directory\n",
		},
		activePerUnit: map[string]string{
			strongswanStarterUnit: "inactive\n",
			ovsIPSecUnit:          "inactive\n",
		},
	}
	t.Cleanup(utils.SetSudoCommandForTest(recorder.stub))

	require.NoError(t, ReconcileOVNIPSec(t.Context(), "/etc/spinifex/spinifex.toml", multiNodeIPSecConfig(false), allReady()))
	assert.Empty(t, recorder.helperRuns())
}

// A nil cluster config means the intent is unknown; tearing down working
// tunnels on a guess would be worse than leaving them.
func TestReconcileOVNIPSec_NilConfigLeavesUnitsAlone(t *testing.T) {
	t.Cleanup(utils.SetSudoCommandForTest(func(name string, args ...string) *exec.Cmd {
		t.Fatalf("utils.SudoCommand must not run with a nil cluster config; got %s %v", name, args)
		return exec.Command("true")
	}))

	require.NoError(t, ReconcileOVNIPSec(t.Context(), "/etc/spinifex/spinifex.toml", nil, allReady()))
}

// A single attempt is what left two of three chassis unconfigured for minutes
// while a peer already required encryption; the pass has to be retried.
func TestMaintainIPSec_RetriesAFailedPass(t *testing.T) {
	recorder := &recordingSudo{}
	t.Cleanup(utils.SetSudoCommandForTest(recorder.stub))

	origRetry, origInterval := ipsecRetryDelay, ipsecReconcileInterval
	ipsecRetryDelay, ipsecReconcileInterval = time.Millisecond, time.Millisecond
	t.Cleanup(func() { ipsecRetryDelay, ipsecReconcileInterval = origRetry, origInterval })

	configPath := filepath.Join(ipsecTestConfigDir(t), "spinifex.toml")
	barrier := &flakyBarrier{failFor: 2}

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan struct{})
	go func() {
		MaintainIPSec(ctx, configPath, multiNodeIPSecConfig(true), barrier)
		close(done)
	}()

	require.Eventually(t, func() bool { return barrier.succeeded() }, 5*time.Second, 5*time.Millisecond,
		"MaintainIPSec gave up after the first failure")

	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("MaintainIPSec did not stop on context cancellation")
	}
}

// flakyBarrier fails its first failFor reads, then reports the cluster ready.
type flakyBarrier struct {
	mu      sync.Mutex
	failFor int
	calls   int
}

func (f *flakyBarrier) PublishLocalReady(context.Context, string, bool) error { return nil }

func (f *flakyBarrier) NodesReady(context.Context, []string) (bool, []string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.calls <= f.failFor {
		return false, nil, errors.New("kv unavailable")
	}
	return true, nil, nil
}

func (f *flakyBarrier) succeeded() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls > f.failFor
}
