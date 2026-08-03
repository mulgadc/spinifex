package handlers_bedrock

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/mulgadc/spinifex/spinifex/awserrors"
	"github.com/mulgadc/spinifex/spinifex/testutil"
	"github.com/mulgadc/spinifex/spinifex/utils"
	"github.com/nats-io/nats.go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// stubNATSHandler mirrors daemon.handleNATSRequest's unmarshal -> service ->
// marshal/error -> respond wire contract, without importing the daemon
// package (which would pull in the whole cluster runtime for one handler
// shape). It exists purely so this test exercises the exact bytes
// NATSEndpointService and the real daemon subscription put on the wire.
func stubNATSHandler[I any, O any](serviceFn func(context.Context, *I, string) (*O, error)) nats.MsgHandler {
	return func(msg *nats.Msg) {
		accountID := utils.AccountIDFromMsg(msg)
		input := new(I)
		if errResp := utils.UnmarshalJsonPayload(input, msg.Data); errResp != nil {
			_ = msg.Respond(errResp)
			return
		}
		output, err := serviceFn(context.Background(), input, accountID)
		if err != nil {
			payload := utils.GenerateErrorPayloadWithMessage(awserrors.ValidErrorCodeFromError(err), err.Error())
			_ = msg.Respond(payload)
			return
		}
		data, err := json.Marshal(output)
		if err != nil {
			_ = msg.Respond(utils.GenerateErrorPayloadWithMessage(awserrors.ErrorServerInternal, err.Error()))
			return
		}
		_ = msg.Respond(data)
	}
}

// subscribeService registers every EndpointService subject on the
// "spinifex-workers" queue group, exactly as daemon.subscribeAll does for the
// real Service, so the client-side round trip in these tests exercises the
// production subject names and queue group.
func subscribeService(t *testing.T, nc *nats.Conn, svc EndpointService) {
	t.Helper()
	subs := []struct {
		subject string
		handler nats.MsgHandler
	}{
		{SubjectEnsureEndpoint, stubNATSHandler(svc.Ensure)},
		{SubjectDescribeEndpoint, stubNATSHandler(svc.Describe)},
		{SubjectListEndpoints, stubNATSHandler(svc.List)},
		{SubjectDeleteEndpoint, stubNATSHandler(svc.Delete)},
	}
	for _, s := range subs {
		sub, err := nc.QueueSubscribe(s.subject, "spinifex-workers", s.handler)
		require.NoError(t, err)
		t.Cleanup(func() { _ = sub.Unsubscribe() })
	}
}

func TestNATSEndpointService_EnsureDescribeListDelete_RoundTrip(t *testing.T) {
	h := newLaunchHarness()
	svc, nc := newTestService(t, h, http.StatusOK, sufficientGPU())
	subscribeService(t, nc, svc)

	client := NewNATSEndpointService(nc)

	ensureOut, err := client.Ensure(t.Context(), &EnsureEndpointInput{ModelID: testModelID}, "000000000000")
	require.NoError(t, err)
	assert.Equal(t, StateStarting, ensureOut.Endpoint.State)
	assert.Equal(t, testModelID, ensureOut.Endpoint.ModelID)

	svc.WaitLaunches()

	descOut, err := client.Describe(t.Context(), &DescribeEndpointInput{ModelID: testModelID}, "000000000000")
	require.NoError(t, err)
	assert.Equal(t, StateReady, descOut.Endpoint.State)
	assert.NotEmpty(t, descOut.Endpoint.BaseURL)

	listOut, err := client.List(t.Context(), &ListEndpointsInput{}, "000000000000")
	require.NoError(t, err)
	require.Len(t, listOut.Endpoints, 1)
	assert.Equal(t, testModelID, listOut.Endpoints[0].ModelID)

	_, err = client.Delete(t.Context(), &DeleteEndpointInput{ModelID: testModelID}, "000000000000")
	require.NoError(t, err)

	descOut, err = client.Describe(t.Context(), &DescribeEndpointInput{ModelID: testModelID}, "000000000000")
	require.NoError(t, err)
	assert.Equal(t, StateAbsent, descOut.Endpoint.State)
}

// TestNATSEndpointService_EnsureError_CarriesCodeAndMessage confirms a
// service-side awserrors.Errorf refusal survives the NATS round trip with
// both its wire code and its human message intact, not just a bare code.
func TestNATSEndpointService_EnsureError_CarriesCodeAndMessage(t *testing.T) {
	h := newLaunchHarness()
	svc, nc := newTestService(t, h, http.StatusOK, &stubSnapshotter{}) // no free capacity
	subscribeService(t, nc, svc)

	client := NewNATSEndpointService(nc)
	_, err := client.Ensure(t.Context(), &EnsureEndpointInput{ModelID: testModelID}, "000000000000")
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorModelNotReadyException, awserrors.ValidErrorCodeFromError(err))
	assert.Contains(t, err.Error(), "VRAM", "the refusal's human message must survive the NATS round trip, not just its code")
}

func TestNATSEndpointService_NoResponder(t *testing.T) {
	_, nc, _ := testutil.StartTestJetStream(t)
	client := NewNATSEndpointService(nc)

	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	_, err := client.Describe(ctx, &DescribeEndpointInput{ModelID: testModelID}, "000000000000")
	require.Error(t, err)
}
