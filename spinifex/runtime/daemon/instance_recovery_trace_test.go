//test:in-package — attempt and the span it opens are unexported, and the span is
//the whole point: it is what ties a recovery's phases to the attempt that asked
//for them.

package daemon

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/attribute"
)

// The phases of a launch already trace themselves. Without a span over the
// attempt they are orphans: three unrelated traces per recovery, no way to ask
// which phase a slow recovery spent its time in, and nothing naming the node it
// came from. So the span is the correlation, and its attributes are the answer to
// "what was this".
func TestARecoveryAttemptIsOneTraceFromClaimToLaunch(t *testing.T) {
	sr := withRecordedSpans(t)

	r := recoveryFixture("node-2")
	// No KV bucket, so the claim fails immediately. The span is opened before the
	// claim and ended by defer, which is exactly what has to hold on the path
	// that gives up earliest.
	r.daemon.jsManager = &JetStreamManager{}
	r.attempt(t.Context(), recoveryCandidate{record: runningOn("i-1", "node-1"), from: "node-1"})

	spans := sr.Ended()
	require.Len(t, spans, 1)
	assert.Equal(t, "recovery.attempt", spans[0].Name())

	got := map[attribute.Key]string{}
	for _, kv := range spans[0].Attributes() {
		got[kv.Key] = kv.Value.AsString()
	}
	assert.Equal(t, "i-1", got["instance.id"])
	assert.Equal(t, "node-1", got["recovery.from"],
		"a recovery is only readable if the trace says where the guest came from")
	assert.Equal(t, "node-2", got["recovery.to"])
}
