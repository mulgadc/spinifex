package viperblockd

// The contract suite run against the provider production actually uses, over
// a real predastore and a real nbdkit export. Until this existed viperblockd
// was judged only by a live cluster run, so CI's only external witness to the
// contract was qemunbdd.

import (
	"os"
	"os/exec"
	"strconv"
	"testing"
	"time"

	"github.com/mulgadc/spinifex/spinifex/ebsprovider"
	"github.com/mulgadc/spinifex/spinifex/ebsprovider/conformance"
	"github.com/mulgadc/spinifex/spinifex/types"
	testpredastore "github.com/mulgadc/spinifex/tests/fixtures/predastore"
)

// pluginPathEnvVar names the built nbdkit plugin (viperblock's
// lib/nbdkit-viperblock-plugin.so, `make plugin` in that repo). It has no
// default: the path depends on where viperblock is checked out.
const pluginPathEnvVar = "VIPERBLOCK_NBDKIT_PLUGIN"

// requireExportTools skips unless this host can actually publish an export.
// SPINIFEX_REQUIRE_CONFORMANCE_TOOLS turns the skip into a failure so a CI
// image that loses nbdkit cannot quietly stop running this suite.
func requireExportTools(t *testing.T) string {
	t.Helper()
	required := os.Getenv("SPINIFEX_REQUIRE_CONFORMANCE_TOOLS") != ""
	fail := func(format string, args ...any) {
		t.Helper()
		if required {
			t.Fatalf(format, args...)
		}
		t.Skipf(format, args...)
	}

	if _, err := exec.LookPath("nbdkit"); err != nil {
		fail("nbdkit not installed")
	}
	pluginPath := os.Getenv(pluginPathEnvVar)
	if pluginPath == "" {
		fail("%s is not set to the built nbdkit plugin", pluginPathEnvVar)
	}
	if _, err := os.Stat(pluginPath); err != nil {
		fail("%s=%s is not readable: %v", pluginPathEnvVar, pluginPath, err)
	}
	return pluginPath
}

// inProcessProvider stands the provider handlers up over an embedded NATS and
// a real predastore, and returns a client for them plus the node they answer
// as. Everything is torn down with the test.
func inProcessProvider(t *testing.T) (func(t *testing.T) ebsprovider.EBSProvider, string) {
	t.Helper()
	pluginPath := requireExportTools(t)

	fixture := testpredastore.Start(t)
	_, natsURL := setupEmbeddedNATS(t)

	cfg := setupTestConfig(t, natsURL)
	cfg.S3Host = "https://" + fixture.Host
	cfg.Bucket = testpredastore.DefaultBucket
	cfg.Region = fixture.Region
	cfg.AccessKey = fixture.AccessKey
	cfg.SecretKey = fixture.SecretKey
	cfg.PluginPath = pluginPath
	cfg.NBDTransport = types.NBDTransportTCP
	cfg.NodeName = "node-1"

	nc := startProviderSubjects(t, cfg, natsURL)

	return func(t *testing.T) ebsprovider.EBSProvider {
		t.Helper()
		return ebsprovider.NewNATSProvider(nc, 60*time.Second)
	}, cfg.NodeName
}

// runPrefix names one run's volumes so it cannot meet an earlier run's
// leftovers: the predastore fixture's object store outlives a single test.
func runPrefix() string {
	return "ci" + strconv.FormatInt(time.Now().UnixNano(), 10) + "-"
}

// TestViperblockdConformance judges the provider handlers by the same suite
// MemoryProvider and qemunbdd answer to.
func TestViperblockdConformance(t *testing.T) {
	newProvider, nodeID := inProcessProvider(t)
	conformance.RunSuiteWithConfig(t, newProvider, conformance.SuiteConfig{
		NamePrefix: runPrefix(),
		NodeID:     nodeID,
	})
}

// TestViperblockdNBDClient checks the half of the boundary our own Go client
// cannot: whether the nbdkit export viperblockd publishes is usable by libnbd,
// which knows nothing about this codebase.
func TestViperblockdNBDClient(t *testing.T) {
	newProvider, nodeID := inProcessProvider(t)
	conformance.RunNBDClientSuiteWithConfig(t, newProvider, conformance.NBDClientConfig{
		NodeID:       nodeID,
		VolumePrefix: "vol-" + runPrefix() + "nbd",
	})
}
