package conformance_test

import (
	"testing"

	"github.com/mulgadc/spinifex/internal/testkit"
	"github.com/mulgadc/spinifex/spinifex/providers/ebs/conformance"
	"github.com/mulgadc/spinifex/spinifex/providers/ebs/natsserve"
	"github.com/mulgadc/spinifex/spinifex/providers/ebs/nullprovider"
)

// BenchmarkNullProvider_InProcess prices the contract with no transport and no
// storage. Anything a real provider costs above this is its own.
func BenchmarkNullProvider_InProcess(b *testing.B) {
	conformance.RunBenchSuite(b, nullprovider.New(), conformance.BenchConfig{})
}

// BenchmarkNullProvider_OverNATS adds the transport and nothing else, so the
// gap between it and the in-process run is what the seam costs: encode,
// publish, queue dispatch, decode.
func BenchmarkNullProvider_OverNATS(b *testing.B) {
	_, conn := testutil.StartTestNATS(b)
	client, stop, err := nullprovider.Serve(b.Context(), conn, natsserve.Options{NoQueueGroup: true})
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(stop)
	conformance.RunBenchSuite(b, client, conformance.BenchConfig{})
}
