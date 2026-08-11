package viperblockd

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/mulgadc/spinifex/spinifex/testutil"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/trace"
)

// emptyBucketS3Host answers ListObjectsV2 with an empty bucket and everything
// else with 404. Init therefore succeeds and LoadState is reached and fails,
// which is what puts both legs of an engine open on the wire.
func emptyBucketS3Host(t *testing.T) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("list-type") == "2" {
			w.Header().Set("Content-Type", "application/xml")
			_, _ = w.Write([]byte(`<?xml version="1.0" encoding="UTF-8"?><ListBucketResult xmlns="http://s3.amazonaws.com/doc/2006-03-01/"><Name>test-bucket</Name><KeyCount>0</KeyCount><IsTruncated>false</IsTruncated></ListBucketResult>`))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

// TestOpenVolumeVB_S3TimeJoinsTheCallersTrace is why openVolumeVB takes a
// context. Both S3 clients only emit a span when the request already carries
// one, so opening an engine through the no-context Init and LoadState leaves
// every byte of that time unattributable.
//
// Two spans is the assertion that matters: one leg traced and the other not
// would still pass a "some span exists" check while hiding half the cost.
func TestOpenVolumeVB_S3TimeJoinsTheCallersTrace(t *testing.T) {
	recorder := testutil.RecordSpans(t)
	_, natsURL := setupEmbeddedNATS(t)
	cfg := setupTestConfig(t, natsURL)
	cfg.S3Host = emptyBucketS3Host(t)

	ctx, caller := otel.Tracer("test").Start(t.Context(), "caller")
	// The open is expected to fail on a bucket with no state in it. What is
	// under test is which spans it emitted on the way, not whether it opened.
	vb, err := openVolumeVB(ctx, cfg, "vol-tracedopen000001")
	require.Error(t, err)
	require.Nil(t, vb)
	caller.End()

	var joined int
	for _, span := range recorder.Ended() {
		if span.SpanKind() != trace.SpanKindClient {
			continue
		}
		if span.SpanContext().TraceID() == caller.SpanContext().TraceID() {
			joined++
		}
	}
	require.GreaterOrEqual(t, joined, 2,
		"an engine open must put both its backend init and its state load in the caller's trace; got %d S3 client spans", joined)
}
