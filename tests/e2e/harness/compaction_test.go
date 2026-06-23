//go:build e2e

package harness

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// stubSSH records the last command and returns a fixed response, isolating the
// du/poll logic from a live cluster.
type stubSSH struct {
	mu      sync.Mutex
	lastCmd string
	out     []byte
	err     error
}

func (s *stubSSH) Run(_ context.Context, _ Node, cmd string) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lastCmd = cmd
	return s.out, s.err
}

func (s *stubSSH) Close() error { return nil }

func oneNodeCluster() *Cluster {
	return &Cluster{Nodes: []Node{{Index: 1, Name: "node1", Addr: "10.0.0.1"}}}
}

func TestChurnObjectCount_ScalesWithBaseline(t *testing.T) {
	baseline := int64(100) * churnObjectBytes

	got := churnObjectCount(baseline)

	assert.Equal(t, int(baseline*churnBaselineMultiple/churnObjectBytes), got)
	assert.Equal(t, churnMinObjects, churnObjectCount(0), "tiny baseline must hit the floor")
}

func TestShardStoreUsage_TargetsShardDirsNotIndex(t *testing.T) {
	ssh := &stubSSH{out: []byte("123\n" + duSentinel + "\n")}

	total, err := shardStoreUsageBytes(context.Background(), ssh, oneNodeCluster())

	require.NoError(t, err)
	assert.Equal(t, int64(123), total)
	assert.Contains(t, ssh.lastCmd, "distributed/nodes/node-*")
	assert.NotContains(t, ssh.lastCmd, "/db")
	assert.Contains(t, ssh.lastCmd, duSentinel, "command must carry the transport sentinel")
}

// An empty du measurement is anomalous (awk floors to "0") but must not crash
// the gate: the sentinel proves the output arrived, so empty counts as 0.
func TestShardStoreUsage_EmptyMeasurementIsZero(t *testing.T) {
	ssh := &stubSSH{out: []byte(duSentinel + "\n")}

	total, err := shardStoreUsageBytes(context.Background(), ssh, oneNodeCluster())

	require.NoError(t, err)
	assert.Equal(t, int64(0), total)
}

// A missing sentinel means the output was lost in SSH retrieval — surface that
// as a transport error rather than a bare ParseInt failure.
func TestShardStoreUsage_MissingSentinelIsTransportError(t *testing.T) {
	ssh := &stubSSH{out: []byte("")}

	_, err := shardStoreUsageBytes(context.Background(), ssh, oneNodeCluster())

	require.Error(t, err)
	assert.Contains(t, err.Error(), "lost in SSH retrieval")
}

func TestPollUntilSettled_FailsAtDeadline(t *testing.T) {
	ssh := &stubSSH{out: []byte("1000\n" + duSentinel + "\n")}

	_, err := pollUntilSettled(context.Background(), ssh, oneNodeCluster(), 500, 50*time.Millisecond, 10*time.Millisecond)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "did not settle")
}
