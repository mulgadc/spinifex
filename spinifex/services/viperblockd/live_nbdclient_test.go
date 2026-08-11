// This runs the external NBD client suite against a real viperblockd over a
// live cluster's NATS, so the nbdkit export is judged by libnbd rather than
// by our own client. Opt-in: SPINIFEX_VIPERBLOCK_LIVE=1, on the node itself.
package viperblockd_test

import (
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/mulgadc/spinifex/spinifex/admin"
	"github.com/mulgadc/spinifex/spinifex/config"
	"github.com/mulgadc/spinifex/spinifex/ebsprovider"
	"github.com/mulgadc/spinifex/spinifex/ebsprovider/conformance"
	"github.com/mulgadc/spinifex/spinifex/utils"
	"github.com/stretchr/testify/require"
)

const liveEnvVar = "SPINIFEX_VIPERBLOCK_LIVE"

// defaultConfigPath mirrors the path the systemd unit sets for
// SPINIFEX_CONFIG_PATH when the operator does not override it.
const defaultConfigPath = "/etc/spinifex/spinifex.toml"

// liveVolumeBytes is one nbdkit chunk-friendly size that is still quick to
// round trip over the real store.
const liveVolumeBytes int64 = 64 << 20

// liveProvider dials the running cluster's NATS and returns a provider client
// pointed at the local viperblockd, plus the node it is running on.
func liveProvider(t *testing.T) (func(t *testing.T) ebsprovider.EBSProvider, string) {
	t.Helper()
	if os.Getenv(liveEnvVar) == "" {
		t.Skipf("skipping: set %s=1 to run this against a real viperblockd over a live cluster's NATS", liveEnvVar)
	}

	cfgPath := os.Getenv("SPINIFEX_CONFIG_PATH")
	if cfgPath == "" {
		cfgPath = defaultConfigPath
	}

	clusterConfig, err := config.LoadConfig(cfgPath)
	require.NoError(t, err, "load cluster config from %s", cfgPath)

	nodeID := clusterConfig.Node
	require.NotEmpty(t, nodeID, "cluster config has no node name set")
	nodeConfig, ok := clusterConfig.Nodes[nodeID]
	require.True(t, ok, "cluster config has no entry for node %q", nodeID)
	t.Logf("node %q, config %s", nodeID, cfgPath)

	// NATS token and CA cert go straight from config into the connect
	// helper. Never log them, and never let a failure message embed them.
	nc, err := utils.ConnectNATSWithRetry(admin.DialTarget(nodeConfig.NATS.Host), nodeConfig.NATS.ACL.Token, nodeConfig.NATS.CACert)
	require.NoError(t, err, "connect to NATS")
	t.Cleanup(nc.Close)

	return func(t *testing.T) ebsprovider.EBSProvider {
		t.Helper()
		return ebsprovider.NewNATSProvider(nc, 120*time.Second)
	}, nodeID
}

// runPrefix names one run's volumes so it cannot collide with an earlier run's
// leftovers on a store that keeps them.
func runPrefix(kind string) string {
	return kind + strconv.FormatInt(time.Now().UnixNano(), 10) + "-"
}

// TestLive_ViperblockdExternalNBDClient points libnbd at whatever nbdkit
// export viperblockd publishes, through the same NATS client shape the
// control plane uses.
func TestLive_ViperblockdExternalNBDClient(t *testing.T) {
	newProvider, nodeID := liveProvider(t)
	conformance.RunNBDClientSuiteWithConfig(t, newProvider,
		conformance.NBDClientConfig{
			NodeID:       nodeID,
			VolumePrefix: "vol-nbdlive-" + strconv.FormatInt(time.Now().UnixNano(), 10),
			VolumeBytes:  liveVolumeBytes,
		})
}

// TestLive_ViperblockdConformance runs the contract suite against the real
// viperblockd. Until now it had only ever run against MemoryProvider and
// qemunbdd, so the provider production actually uses was the one implementation
// the suite never judged.
func TestLive_ViperblockdConformance(t *testing.T) {
	newProvider, nodeID := liveProvider(t)
	conformance.RunSuiteWithConfig(t, newProvider, conformance.SuiteConfig{
		NamePrefix: runPrefix("live"),
		NodeID:     nodeID,
	})
}
