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

// inProcessProvider stands two nodes of the provider handlers up over one
// embedded NATS and one predastore, and returns a client for them plus both
// node names. Everything is torn down with the test.
//
// Two nodes rather than one because the exclusion this provider advertises is
// cluster-scoped, and a single node cannot exercise it: the second publish
// would go to a subject nobody serves and time out instead of being refused.
// Both share the NATS the volume leases live in and the bucket the volumes
// live in, so a second opener is refused by the lease, not by the harness.
func inProcessProvider(t *testing.T) (func(t *testing.T) ebsprovider.EBSProvider, string, string) {
	t.Helper()
	pluginPath := requireExportTools(t)

	fixture := testpredastore.Start(t)
	_, natsURL := setupEmbeddedNATS(t)

	newNode := func(name string) *Config {
		cfg := setupTestConfig(t, natsURL)
		cfg.S3Host = "https://" + fixture.Host
		cfg.Bucket = testpredastore.DefaultBucket
		cfg.Region = fixture.Region
		cfg.AccessKey = fixture.AccessKey
		cfg.SecretKey = fixture.SecretKey
		cfg.PluginPath = pluginPath
		cfg.NBDTransport = types.NBDTransportTCP
		cfg.NodeName = name
		// The lease owner is derived from NodeName, and setupTestConfig built
		// the store before this function could set it. Rebuild it, or both
		// nodes claim as the same owner and each reclaims the other's lease
		// as if it were its own stale entry.
		installTestVolumeLeases(t, cfg, natsURL)
		return cfg
	}

	first := newNode("node-1")
	second := newNode("node-2")
	nc := startProviderSubjects(t, first, natsURL)
	startProviderSubjects(t, second, natsURL)

	return func(t *testing.T) ebsprovider.EBSProvider {
		t.Helper()
		return ebsprovider.NewNATSProvider(nc, 60*time.Second)
	}, first.NodeName, second.NodeName
}

// runPrefix names one run's volumes so it cannot meet an earlier run's
// leftovers: the predastore fixture's object store outlives a single test.
func runPrefix() string {
	return "ci" + strconv.FormatInt(time.Now().UnixNano(), 10) + "-"
}

// TestViperblockdConformance judges the provider handlers by the same suite
// MemoryProvider and qemunbdd answer to.
func TestViperblockdConformance(t *testing.T) {
	newProvider, nodeID, otherNodeID := inProcessProvider(t)
	conformance.RunSuiteWithConfig(t, newProvider, conformance.SuiteConfig{
		NamePrefix:  runPrefix(),
		NodeID:      nodeID,
		OtherNodeID: otherNodeID,
	})
}

// TestViperblockdNBDClient checks the half of the boundary our own Go client
// cannot: whether the nbdkit export viperblockd publishes is usable by libnbd,
// which knows nothing about this codebase.
func TestViperblockdNBDClient(t *testing.T) {
	newProvider, nodeID, _ := inProcessProvider(t)
	conformance.RunNBDClientSuiteWithConfig(t, newProvider, conformance.NBDClientConfig{
		NodeID:       nodeID,
		VolumePrefix: "vol-" + runPrefix() + "nbd",
	})
}
