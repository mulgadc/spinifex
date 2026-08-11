// This prices guest I/O against a real viperblockd over a live cluster's
// NATS, which the provider-verb benchmarks cannot: guest reads and writes
// never cross those verbs. Opt-in: SPINIFEX_VIPERBLOCK_LIVE=1, on the node.
package viperblockd_test

import (
	"strconv"
	"testing"
	"time"

	"github.com/mulgadc/spinifex/spinifex/ebsprovider/conformance"
)

// TestLive_ViperblockdDataPath drives qemu-img bench against the published
// export. It is a measurement, not a contract: it asserts only that the I/O
// completes, and logs the table.
func TestLive_ViperblockdDataPath(t *testing.T) {
	newProvider, nodeID := liveProvider(t)
	conformance.RunDataPathSuite(t, newProvider, conformance.NBDClientConfig{
		NodeID:       nodeID,
		VolumePrefix: "vol-dpath-" + strconv.FormatInt(time.Now().UnixNano(), 10),
	})
}
