// This prices the provider verbs against a real viperblockd over a live
// cluster's NATS. Opt-in: SPINIFEX_VIPERBLOCK_LIVE=1, on the node itself.
package viperblockd_test

import (
	"testing"

	"github.com/mulgadc/spinifex/spinifex/ebsprovider/conformance"
)

// BenchmarkLive_Viperblockd runs the same suite the null and qemunbdd
// providers run, so the three sets of numbers differ only in what answers.
//
// It measures control operations. Guest I/O never crosses these verbs, so
// nothing here says anything about viperblock's data-path throughput.
func BenchmarkLive_Viperblockd(b *testing.B) {
	provider, nodeID := liveProviderTB(b)
	conformance.RunBenchSuite(b, provider, conformance.BenchConfig{
		SuiteConfig: conformance.SuiteConfig{
			NamePrefix: runPrefix("bench"),
			NodeID:     nodeID,
		},
		VolumeBytes: liveVolumeBytes,
	})
}
