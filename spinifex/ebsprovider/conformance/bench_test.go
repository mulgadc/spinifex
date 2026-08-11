package conformance_test

import (
	"testing"

	"github.com/mulgadc/spinifex/spinifex/ebsprovider/conformance"
	"github.com/mulgadc/spinifex/spinifex/ebsprovider/natsserve"
	"github.com/mulgadc/spinifex/spinifex/ebsprovider/nullprovider"
	"github.com/mulgadc/spinifex/spinifex/testutil"
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
