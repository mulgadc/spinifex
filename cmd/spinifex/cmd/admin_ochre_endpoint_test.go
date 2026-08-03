package cmd

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	handlers_bedrock "github.com/mulgadc/spinifex/spinifex/handlers/bedrock"
	"github.com/stretchr/testify/require"
)

// fakeEndpointService serves scripted Describe responses so the wait loop can
// be exercised without a daemon, a VM or a real cold start.
type fakeEndpointService struct {
	ensure      handlers_bedrock.EndpointRecord
	ensureErr   error
	describes   []handlers_bedrock.EndpointRecord
	describeErr error
	list        []handlers_bedrock.EndpointRecord
	listErr     error
	deleteErr   error

	describeCalls int
	deleteCalls   int
}

func (f *fakeEndpointService) Ensure(_ context.Context, _ *handlers_bedrock.EnsureEndpointInput, _ string) (*handlers_bedrock.EnsureEndpointOutput, error) {
	if f.ensureErr != nil {
		return nil, f.ensureErr
	}
	return &handlers_bedrock.EnsureEndpointOutput{Endpoint: f.ensure}, nil
}

func (f *fakeEndpointService) Describe(_ context.Context, _ *handlers_bedrock.DescribeEndpointInput, _ string) (*handlers_bedrock.DescribeEndpointOutput, error) {
	if f.describeErr != nil {
		return nil, f.describeErr
	}
	// The last scripted response repeats, so a test that never reaches a
	// terminal state keeps polling rather than running off the end.
	idx := min(f.describeCalls, len(f.describes)-1)
	f.describeCalls++
	return &handlers_bedrock.DescribeEndpointOutput{Endpoint: f.describes[idx]}, nil
}

func (f *fakeEndpointService) List(_ context.Context, _ *handlers_bedrock.ListEndpointsInput, _ string) (*handlers_bedrock.ListEndpointsOutput, error) {
	if f.listErr != nil {
		return nil, f.listErr
	}
	return &handlers_bedrock.ListEndpointsOutput{Endpoints: f.list}, nil
}

func (f *fakeEndpointService) Delete(_ context.Context, _ *handlers_bedrock.DeleteEndpointInput, _ string) (*handlers_bedrock.DeleteEndpointOutput, error) {
	f.deleteCalls++
	if f.deleteErr != nil {
		return nil, f.deleteErr
	}
	return &handlers_bedrock.DeleteEndpointOutput{}, nil
}

var _ handlers_bedrock.EndpointService = (*fakeEndpointService)(nil)

// fakeClock advances a virtual now by whatever the loop sleeps, so a
// multi-minute wait costs nothing and elapsed times are exact.
func fakeClock(now *time.Time) endpointWaitClock {
	return endpointWaitClock{
		now:   func() time.Time { return *now },
		sleep: func(d time.Duration) { *now = now.Add(d) },
	}
}

const testModelID = "meta.llama3-2-1b-instruct-v1:0"

func TestWaitForEndpointReady_ReachesReadyAndReportsElapsed(t *testing.T) {
	svc := &fakeEndpointService{describes: []handlers_bedrock.EndpointRecord{
		{ModelID: testModelID, State: handlers_bedrock.StateStarting},
		{ModelID: testModelID, State: handlers_bedrock.StateStarting},
		{ModelID: testModelID, State: handlers_bedrock.StateReady, BaseURL: "http://10.0.0.5:8000"},
	}}
	now := time.Unix(0, 0).UTC()

	rec, elapsed, err := waitForEndpointReady(context.Background(), svc, testModelID, time.Minute, fakeClock(&now))
	require.NoError(t, err)
	require.Equal(t, handlers_bedrock.StateReady, rec.State)
	require.Equal(t, "http://10.0.0.5:8000", rec.BaseURL)
	// Two sleeps between three polls.
	require.Equal(t, 2*endpointPollInterval, elapsed)
}

// A failed launch deletes the record, so ABSENT after STARTING is the failure
// signal and must not be mistaken for "not started yet".
func TestWaitForEndpointReady_AbsentAfterStartingIsAbort(t *testing.T) {
	svc := &fakeEndpointService{describes: []handlers_bedrock.EndpointRecord{
		{ModelID: testModelID, State: handlers_bedrock.StateStarting},
		{ModelID: testModelID, State: handlers_bedrock.StateAbsent},
	}}
	now := time.Unix(0, 0).UTC()

	_, _, err := waitForEndpointReady(context.Background(), svc, testModelID, time.Minute, fakeClock(&now))
	require.ErrorIs(t, err, errEndpointLaunchAborted)
}

func TestWaitForEndpointReady_TimesOutWhileStillStarting(t *testing.T) {
	svc := &fakeEndpointService{describes: []handlers_bedrock.EndpointRecord{
		{ModelID: testModelID, State: handlers_bedrock.StateStarting},
	}}
	now := time.Unix(0, 0).UTC()

	rec, elapsed, err := waitForEndpointReady(context.Background(), svc, testModelID, 10*time.Second, fakeClock(&now))
	require.ErrorIs(t, err, errEndpointWaitTimeout)
	require.Equal(t, handlers_bedrock.StateStarting, rec.State)
	require.GreaterOrEqual(t, elapsed, 10*time.Second)
}

// A zero timeout must still report the endpoint's real state, not an empty
// record, because the deadline is only checked after a Describe.
func TestWaitForEndpointReady_ZeroTimeoutStillDescribesOnce(t *testing.T) {
	svc := &fakeEndpointService{describes: []handlers_bedrock.EndpointRecord{
		{ModelID: testModelID, State: handlers_bedrock.StateStarting},
	}}
	now := time.Unix(0, 0).UTC()

	rec, _, err := waitForEndpointReady(context.Background(), svc, testModelID, 0, fakeClock(&now))
	require.ErrorIs(t, err, errEndpointWaitTimeout)
	require.Equal(t, handlers_bedrock.StateStarting, rec.State)
	require.Equal(t, 1, svc.describeCalls)
}

func TestWaitForEndpointReady_DescribeErrorSurfaces(t *testing.T) {
	svc := &fakeEndpointService{describeErr: errors.New("nats: timeout")}
	now := time.Unix(0, 0).UTC()

	_, _, err := waitForEndpointReady(context.Background(), svc, testModelID, time.Minute, fakeClock(&now))
	require.ErrorContains(t, err, "nats: timeout")
}

func TestRunEnsureEndpoint_NoWaitReportsStarting(t *testing.T) {
	svc := &fakeEndpointService{ensure: handlers_bedrock.EndpointRecord{ModelID: testModelID, State: handlers_bedrock.StateStarting}}
	now := time.Unix(0, 0).UTC()

	msg, err := runEnsureEndpoint(context.Background(), svc, testModelID, false, time.Minute, fakeClock(&now))
	require.NoError(t, err)
	require.Contains(t, msg, "STARTING")
	require.Equal(t, 0, svc.describeCalls, "no --wait must not poll")
}

func TestRunEnsureEndpoint_WaitReportsColdStart(t *testing.T) {
	svc := &fakeEndpointService{
		ensure: handlers_bedrock.EndpointRecord{ModelID: testModelID, State: handlers_bedrock.StateStarting},
		describes: []handlers_bedrock.EndpointRecord{
			{ModelID: testModelID, State: handlers_bedrock.StateStarting},
			{ModelID: testModelID, State: handlers_bedrock.StateReady},
		},
	}
	now := time.Unix(0, 0).UTC()

	msg, err := runEnsureEndpoint(context.Background(), svc, testModelID, true, time.Minute, fakeClock(&now))
	require.NoError(t, err)
	require.Contains(t, msg, "READY after 2s")
}

// An endpoint already READY when ensure returns was a warm request; reporting
// an elapsed cold start for it would be misleading.
func TestRunEnsureEndpoint_AlreadyReadyDoesNotReportElapsed(t *testing.T) {
	svc := &fakeEndpointService{ensure: handlers_bedrock.EndpointRecord{ModelID: testModelID, State: handlers_bedrock.StateReady}}
	now := time.Unix(0, 0).UTC()

	msg, err := runEnsureEndpoint(context.Background(), svc, testModelID, true, time.Minute, fakeClock(&now))
	require.NoError(t, err)
	require.Contains(t, msg, "already READY")
	require.NotContains(t, msg, "after")
	require.Equal(t, 0, svc.describeCalls)
}

func TestRunEnsureEndpoint_EnsureErrorSurfaces(t *testing.T) {
	svc := &fakeEndpointService{ensureErr: errors.New("ResourceNotFoundException")}
	now := time.Unix(0, 0).UTC()

	_, err := runEnsureEndpoint(context.Background(), svc, "no.such.model", false, time.Minute, fakeClock(&now))
	require.ErrorContains(t, err, "ResourceNotFoundException")
}

// A timeout must name the state the endpoint was left in, so an operator can
// tell a slow launch from a stuck one.
func TestRunEnsureEndpoint_WaitTimeoutIncludesRecord(t *testing.T) {
	svc := &fakeEndpointService{
		ensure:    handlers_bedrock.EndpointRecord{ModelID: testModelID, State: handlers_bedrock.StateStarting},
		describes: []handlers_bedrock.EndpointRecord{{ModelID: testModelID, State: handlers_bedrock.StateStarting}},
	}
	now := time.Unix(0, 0).UTC()

	_, err := runEnsureEndpoint(context.Background(), svc, testModelID, true, 4*time.Second, fakeClock(&now))
	require.ErrorIs(t, err, errEndpointWaitTimeout)
	require.ErrorContains(t, err, "STARTING")
}

func TestListEndpointsOutput_NoEndpoints(t *testing.T) {
	svc := &fakeEndpointService{}
	msg, err := listEndpointsOutput(context.Background(), svc)
	require.NoError(t, err)
	require.Equal(t, "No serving endpoints.", msg)
}

func TestListEndpointsOutput_ListsEndpoints(t *testing.T) {
	svc := &fakeEndpointService{list: []handlers_bedrock.EndpointRecord{
		{ModelID: testModelID, State: handlers_bedrock.StateReady, InstanceID: "i-abc", BaseURL: "http://10.0.0.5:8000"},
		{ModelID: "meta.llama3-2-3b-instruct-v1:0", State: handlers_bedrock.StateStarting},
	}}

	msg, err := listEndpointsOutput(context.Background(), svc)
	require.NoError(t, err)
	require.Contains(t, msg, testModelID)
	require.Contains(t, msg, "i-abc")
	require.Contains(t, msg, "STARTING")
}

func TestListEndpointsOutput_ErrorSurfaces(t *testing.T) {
	svc := &fakeEndpointService{listErr: errors.New("nats: no responders")}
	_, err := listEndpointsOutput(context.Background(), svc)
	require.ErrorContains(t, err, "no responders")
}

func TestFormatEndpointRecord_OmitsUnsetFieldsAndDerivesStartup(t *testing.T) {
	created := time.Unix(1000, 0).UTC()
	rec := handlers_bedrock.EndpointRecord{
		ModelID:   testModelID,
		State:     handlers_bedrock.StateReady,
		CreatedAt: created,
		ReadyAt:   created.Add(97 * time.Second),
		BaseURL:   "http://10.0.0.5:8000",
	}

	out := formatEndpointRecord(rec)
	require.Contains(t, out, "Startup:")
	require.Contains(t, out, "1m37s")
	require.NotContains(t, out, "Instance ID")
	require.NotContains(t, out, "Weights volume")
}

func TestFormatEndpointRecord_AbsentIsMinimal(t *testing.T) {
	out := formatEndpointRecord(handlers_bedrock.EndpointRecord{ModelID: testModelID, State: handlers_bedrock.StateAbsent})
	require.Contains(t, out, "ABSENT")
	require.Equal(t, 2, strings.Count(strings.TrimSpace(out), "\n")+1)
}
