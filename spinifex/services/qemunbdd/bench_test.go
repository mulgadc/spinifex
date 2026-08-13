package qemunbdd_test

// The control for the A/B. qemunbdd shares no code with viperblock and stores
// real qcow2 files, so what it costs above the null provider is what a
// straightforward local implementation of these verbs costs.

import (
	"testing"
	"time"

	"github.com/mulgadc/spinifex/spinifex/ebsprovider"
	"github.com/mulgadc/spinifex/spinifex/ebsprovider/conformance"
	"github.com/mulgadc/spinifex/spinifex/ebsprovider/natsserve"
	"github.com/mulgadc/spinifex/spinifex/testutil"
	"github.com/stretchr/testify/require"
)

// benchVolumeBytes matches the size the live viperblockd benchmark uses, so
// the two sets of numbers are comparable rather than merely adjacent.
const benchVolumeBytes int64 = 64 << 20

func BenchmarkQEMUNBDProvider_InProcess(b *testing.B) {
	requireQEMUTools(b)
	conformance.RunBenchSuite(b, newQEMUProviderTB(b), conformance.BenchConfig{
		VolumeBytes: benchVolumeBytes,
	})
}

// BenchmarkQEMUNBDProvider_OverNATS is the shape the control plane actually
// calls: same verbs, same transport, a provider that does real filesystem
// work. The gap to the in-process run should be the seam and nothing else.
func BenchmarkQEMUNBDProvider_OverNATS(b *testing.B) {
	requireQEMUTools(b)
	_, conn := testutil.StartTestNATS(b)
	stop, err := natsserve.Serve(b.Context(), conn, newQEMUProviderTB(b), natsserve.Options{})
	require.NoError(b, err)
	b.Cleanup(stop)

	conformance.RunBenchSuite(b, ebsprovider.NewNATSProvider(conn, 120*time.Second), conformance.BenchConfig{
		VolumeBytes: benchVolumeBytes,
	})
}
