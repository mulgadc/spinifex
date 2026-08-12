package viperblockd

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/mulgadc/spinifex/spinifex/ebsprovider"
	"github.com/mulgadc/spinifex/spinifex/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/codes"
)

// TestProviderTracing_ServerSpanJoinsTheCallersTrace covers the provider that
// actually runs in production. viperblockd serves the contract with its own
// handlers rather than natsserve, so its propagation is a separate claim.
func TestProviderTracing_ServerSpanJoinsTheCallersTrace(t *testing.T) {
	recorder := testutil.RecordSpans(t)
	_, natsURL := setupEmbeddedNATS(t)
	cfg := setupTestConfig(t, natsURL)
	nc := startProviderSubjects(t, cfg, natsURL)

	client := ebsprovider.NewNATSProvider(nc, 5*time.Second)
	_, err := client.GetCapabilities(t.Context(), ebsprovider.GetCapabilitiesRequest{Versioned: ebsprovider.NewVersioned()})
	require.NoError(t, err)

	clientSpan, serverSpan := testutil.SpansByKind(t, recorder)
	require.NotNil(t, clientSpan, "no client span recorded")
	require.NotNil(t, serverSpan, "no server span recorded")
	assert.Equal(t, "ebs.capabilities", clientSpan.Name())
	assert.Equal(t, "ebs.capabilities", serverSpan.Name())
	assert.Equal(t, clientSpan.SpanContext().TraceID(), serverSpan.SpanContext().TraceID())
	assert.Equal(t, clientSpan.SpanContext().SpanID(), serverSpan.Parent().SpanID())
}

// TestProviderTracing_SpanNamesDropTheVolumeID pins the owner-routed path,
// where the subject carries a volume ID that must not reach the span name.
// The request is version-skewed so it is rejected before any backend work,
// which also exercises the error tagging on a reply built by the handler.
func TestProviderTracing_SpanNamesDropTheVolumeID(t *testing.T) {
	const volumeID = "vol-testtracing0001"
	recorder := testutil.RecordSpans(t)
	_, natsURL := setupEmbeddedNATS(t)
	cfg := setupTestConfig(t, natsURL)
	nc := startProviderSubjects(t, cfg, natsURL)

	ownerSubject, err := ebsprovider.GetVolumeOwnerSubject(volumeID)
	require.NoError(t, err)
	subs := subscribeOwnerSubjects(t.Context(), cfg, nc, volumeID)
	t.Cleanup(func() {
		for _, sub := range subs {
			_ = sub.Unsubscribe()
		}
	})
	require.NoError(t, nc.Flush())

	payload := marshalRequest(t, map[string]any{
		"schema_version": ebsprovider.SchemaVersion + 1,
		"volume_id":      volumeID,
	})
	msg := requestProvider(t, nc, ownerSubject, payload)
	var response versionedErrorResponse
	require.NoError(t, json.Unmarshal(msg.Data, &response))
	require.NotNil(t, response.Error)

	_, serverSpan := testutil.SpansByKind(t, recorder)
	require.NotNil(t, serverSpan, "no server span recorded")
	assert.Equal(t, "ebs.volume.describe", serverSpan.Name())
	assert.NotContains(t, serverSpan.Name(), volumeID)
	assert.Equal(t, ownerSubject, testutil.SpanAttribute(t, serverSpan, "ebs.provider.subject").AsString())
	assert.Equal(t, volumeID, testutil.SpanAttribute(t, serverSpan, "ebs.provider.volume_id").AsString())
	assert.Equal(t, codes.Error, serverSpan.Status().Code)
	assert.Equal(t, string(ebsprovider.ErrorCodeUnsupportedVersion),
		testutil.SpanAttribute(t, serverSpan, "ebs.provider.error_code").AsString())
}
