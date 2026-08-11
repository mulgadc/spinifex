package natsserve_test

import (
	"testing"
	"time"

	"github.com/mulgadc/spinifex/spinifex/ebsprovider"
	"github.com/mulgadc/spinifex/spinifex/ebsprovider/natsserve"
	"github.com/mulgadc/spinifex/spinifex/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

func spansByKind(t *testing.T, recorder *tracetest.SpanRecorder) (client, server sdktrace.ReadOnlySpan) {
	t.Helper()
	client, server = testutil.SpansByKind(t, recorder)
	require.NotNil(t, client, "no client span recorded")
	require.NotNil(t, server, "no server span recorded")
	return client, server
}

func serveMemoryProvider(t *testing.T) *ebsprovider.NATSProvider {
	t.Helper()
	_, conn := testutil.StartTestNATS(t)
	provider := ebsprovider.NewMemoryProvider(ebsprovider.Capabilities{VolumeEnumeration: true})
	stop, err := natsserve.Serve(t.Context(), conn, provider, natsserve.Options{NoQueueGroup: true})
	require.NoError(t, err)
	t.Cleanup(stop)
	return ebsprovider.NewNATSProvider(conn, 5*time.Second)
}

// TestServerSpan_JoinsTheCallersTrace is the property that makes provider
// latency attributable: without it the two halves of one operation are two
// unrelated traces, and time spent in the provider cannot be told from time
// spent on the wire.
func TestServerSpan_JoinsTheCallersTrace(t *testing.T) {
	recorder := testutil.RecordSpans(t)
	client := serveMemoryProvider(t)

	_, err := client.ListVolumes(t.Context(), ebsprovider.ListVolumesRequest{Versioned: ebsprovider.NewVersioned()})
	require.NoError(t, err)

	clientSpan, serverSpan := spansByKind(t, recorder)
	assert.Equal(t, "ebs.volume.list", clientSpan.Name())
	assert.Equal(t, "ebs.volume.list", serverSpan.Name())
	assert.Equal(t, clientSpan.SpanContext().TraceID(), serverSpan.SpanContext().TraceID())
	assert.Equal(t, clientSpan.SpanContext().SpanID(), serverSpan.Parent().SpanID())
}

// TestServerSpan_RecordsTheErrorItReturns keeps a failed verb from looking
// like a successful round trip on the serving side.
func TestServerSpan_RecordsTheErrorItReturns(t *testing.T) {
	recorder := testutil.RecordSpans(t)
	client := serveMemoryProvider(t)

	_, err := client.GetVolume(t.Context(), ebsprovider.GetVolumeRequest{
		Versioned: ebsprovider.NewVersioned(), VolumeID: "vol-absent",
	})
	require.ErrorIs(t, err, ebsprovider.ErrNotFound)

	_, serverSpan := spansByKind(t, recorder)
	assert.Equal(t, codes.Error, serverSpan.Status().Code)
}

// TestServerSpan_SurvivesAnUntracedCaller covers a caller that never installed
// a propagator: the provider must still open its own span rather than drop the
// message or inherit a broken parent.
func TestServerSpan_SurvivesAnUntracedCaller(t *testing.T) {
	recorder := testutil.RecordSpans(t)
	client := serveMemoryProvider(t)
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator())

	_, err := client.ListVolumes(t.Context(), ebsprovider.ListVolumesRequest{Versioned: ebsprovider.NewVersioned()})
	require.NoError(t, err)

	_, serverSpan := spansByKind(t, recorder)
	assert.Equal(t, "ebs.volume.list", serverSpan.Name())
	assert.False(t, serverSpan.Parent().IsValid(), "an untraced caller must not leave a dangling parent")
}
