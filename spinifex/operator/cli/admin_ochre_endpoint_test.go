package cli

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/mulgadc/spinifex/spinifex/domains/ochre"
	"github.com/stretchr/testify/require"
)

// fakeEndpointService serves scripted Describe responses so the wait loop can
// be exercised without a daemon, a VM or a real cold start.
type fakeEndpointService struct {
	ensure        ochre.EndpointRecord
	ensureErr     error
	describes     []ochre.EndpointRecord
	describeErr   error
	list          []ochre.EndpointRecord
	listErr       error
	deleteErr     error
	deleteRemoved bool

	describeCalls  int
	deleteCalls    int
	describeInputs []*ochre.DescribeEndpointInput
	deleteInputs   []*ochre.DeleteEndpointInput
}

func (f *fakeEndpointService) Ensure(_ context.Context, _ *ochre.EnsureEndpointInput, _ string) (*ochre.EnsureEndpointOutput, error) {
	if f.ensureErr != nil {
		return nil, f.ensureErr
	}
	return &ochre.EnsureEndpointOutput{Endpoint: f.ensure}, nil
}

func (f *fakeEndpointService) Describe(_ context.Context, in *ochre.DescribeEndpointInput, _ string) (*ochre.DescribeEndpointOutput, error) {
	f.describeInputs = append(f.describeInputs, in)
	if f.describeErr != nil {
		return nil, f.describeErr
	}
	// The last scripted response repeats, so a test that never reaches a
	// terminal state keeps polling rather than running off the end.
	idx := min(f.describeCalls, len(f.describes)-1)
	f.describeCalls++
	return &ochre.DescribeEndpointOutput{Endpoint: f.describes[idx]}, nil
}

func (f *fakeEndpointService) List(_ context.Context, _ *ochre.ListEndpointsInput, _ string) (*ochre.ListEndpointsOutput, error) {
	if f.listErr != nil {
		return nil, f.listErr
	}
	return &ochre.ListEndpointsOutput{Endpoints: f.list}, nil
}

func (f *fakeEndpointService) Delete(_ context.Context, in *ochre.DeleteEndpointInput, _ string) (*ochre.DeleteEndpointOutput, error) {
	f.deleteCalls++
	f.deleteInputs = append(f.deleteInputs, in)
	if f.deleteErr != nil {
		return nil, f.deleteErr
	}
	return &ochre.DeleteEndpointOutput{Removed: f.deleteRemoved}, nil
}

var _ ochre.EndpointService = (*fakeEndpointService)(nil)

// fakeClock advances a virtual now by whatever the loop sleeps, so a
// multi-minute wait costs nothing and elapsed times are exact.
func fakeClock(now *time.Time) endpointWaitClock {
	return endpointWaitClock{
		now:   func() time.Time { return *now },
		sleep: func(d time.Duration) { *now = now.Add(d) },
	}
}

const testModelID = "meta.llama3-2-1b-instruct-v1:0"

// testAccountID stands in for a caller's real (non-Global) account, mirroring
// domains/ochre's own testAccountID.
const testAccountID = "111111111111"

func TestWaitForEndpointReady_ReachesReadyAndReportsElapsed(t *testing.T) {
	svc := &fakeEndpointService{describes: []ochre.EndpointRecord{
		{ModelID: testModelID, State: ochre.StateStarting},
		{ModelID: testModelID, State: ochre.StateStarting},
		{ModelID: testModelID, State: ochre.StateReady, BaseURL: "http://10.0.0.5:8000"},
	}}
	now := time.Unix(0, 0).UTC()

	rec, elapsed, err := waitForEndpointReady(context.Background(), svc, testModelID, time.Minute, fakeClock(&now))
	require.NoError(t, err)
	require.Equal(t, ochre.StateReady, rec.State)
	require.Equal(t, "http://10.0.0.5:8000", rec.BaseURL)
	// Two sleeps between three polls.
	require.Equal(t, 2*endpointPollInterval, elapsed)
}

// A failed launch deletes the record, so ABSENT after STARTING is the failure
// signal and must not be mistaken for "not started yet".
func TestWaitForEndpointReady_AbsentAfterStartingIsAbort(t *testing.T) {
	svc := &fakeEndpointService{describes: []ochre.EndpointRecord{
		{ModelID: testModelID, State: ochre.StateStarting},
		{ModelID: testModelID, State: ochre.StateAbsent},
	}}
	now := time.Unix(0, 0).UTC()

	_, _, err := waitForEndpointReady(context.Background(), svc, testModelID, time.Minute, fakeClock(&now))
	require.ErrorIs(t, err, errEndpointLaunchAborted)
}

func TestWaitForEndpointReady_TimesOutWhileStillStarting(t *testing.T) {
	svc := &fakeEndpointService{describes: []ochre.EndpointRecord{
		{ModelID: testModelID, State: ochre.StateStarting},
	}}
	now := time.Unix(0, 0).UTC()

	rec, elapsed, err := waitForEndpointReady(context.Background(), svc, testModelID, 10*time.Second, fakeClock(&now))
	require.ErrorIs(t, err, errEndpointWaitTimeout)
	require.Equal(t, ochre.StateStarting, rec.State)
	require.GreaterOrEqual(t, elapsed, 10*time.Second)
}

// A zero timeout must still report the endpoint's real state, not an empty
// record, because the deadline is only checked after a Describe.
func TestWaitForEndpointReady_ZeroTimeoutStillDescribesOnce(t *testing.T) {
	svc := &fakeEndpointService{describes: []ochre.EndpointRecord{
		{ModelID: testModelID, State: ochre.StateStarting},
	}}
	now := time.Unix(0, 0).UTC()

	rec, _, err := waitForEndpointReady(context.Background(), svc, testModelID, 0, fakeClock(&now))
	require.ErrorIs(t, err, errEndpointWaitTimeout)
	require.Equal(t, ochre.StateStarting, rec.State)
	require.Equal(t, 1, svc.describeCalls)
}

func TestWaitForEndpointReady_DescribeErrorSurfaces(t *testing.T) {
	svc := &fakeEndpointService{describeErr: errors.New("nats: timeout")}
	now := time.Unix(0, 0).UTC()

	_, _, err := waitForEndpointReady(context.Background(), svc, testModelID, time.Minute, fakeClock(&now))
	require.ErrorContains(t, err, "nats: timeout")
}

func TestRunEnsureEndpoint_NoWaitReportsStarting(t *testing.T) {
	svc := &fakeEndpointService{ensure: ochre.EndpointRecord{ModelID: testModelID, State: ochre.StateStarting}}
	now := time.Unix(0, 0).UTC()

	msg, err := runEnsureEndpoint(context.Background(), svc, testModelID, false, time.Minute, fakeClock(&now))
	require.NoError(t, err)
	require.Contains(t, msg, "STARTING")
	require.Equal(t, 0, svc.describeCalls, "no --wait must not poll")
}

func TestRunEnsureEndpoint_WaitReportsColdStart(t *testing.T) {
	svc := &fakeEndpointService{
		ensure: ochre.EndpointRecord{ModelID: testModelID, State: ochre.StateStarting},
		describes: []ochre.EndpointRecord{
			{ModelID: testModelID, State: ochre.StateStarting},
			{ModelID: testModelID, State: ochre.StateReady},
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
	svc := &fakeEndpointService{ensure: ochre.EndpointRecord{ModelID: testModelID, State: ochre.StateReady}}
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
		ensure:    ochre.EndpointRecord{ModelID: testModelID, State: ochre.StateStarting},
		describes: []ochre.EndpointRecord{{ModelID: testModelID, State: ochre.StateStarting}},
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
	svc := &fakeEndpointService{list: []ochre.EndpointRecord{
		{ModelID: testModelID, State: ochre.StateReady, InstanceID: "i-abc", BaseURL: "http://10.0.0.5:8000"},
		{ModelID: "meta.llama3-2-3b-instruct-v1:0", State: ochre.StateStarting},
	}}

	msg, err := listEndpointsOutput(context.Background(), svc)
	require.NoError(t, err)
	require.Contains(t, msg, testModelID)
	require.Contains(t, msg, "i-abc")
	require.Contains(t, msg, "STARTING")
}

// TestListEndpointsOutput_ShowsAccountAndPinnedColumns is Bug 2's list seam:
// a pinned, account-scoped endpoint must render its own account and a PINNED
// indicator, while a bare GlobalAccountID endpoint keeps listing unchanged
// (its ACCOUNT cell just reads GlobalAccountID, PINNED empty).
func TestListEndpointsOutput_ShowsAccountAndPinnedColumns(t *testing.T) {
	svc := &fakeEndpointService{list: []ochre.EndpointRecord{
		{ModelID: testModelID, State: ochre.StateReady, AccountID: "000000000001"},
		{ModelID: "meta.llama3-2-3b-instruct-v1:0", State: ochre.StateReady, AccountID: testAccountID, Pinned: true},
	}}

	msg, err := listEndpointsOutput(context.Background(), svc)
	require.NoError(t, err)
	require.Contains(t, msg, "ACCOUNT")
	require.Contains(t, msg, "PINNED")
	require.Contains(t, msg, "000000000001")
	require.Contains(t, msg, testAccountID)
}

func TestListEndpointsOutput_ErrorSurfaces(t *testing.T) {
	svc := &fakeEndpointService{listErr: errors.New("nats: no responders")}
	_, err := listEndpointsOutput(context.Background(), svc)
	require.ErrorContains(t, err, "no responders")
}

func TestFormatEndpointRecord_OmitsUnsetFieldsAndDerivesStartup(t *testing.T) {
	created := time.Unix(1000, 0).UTC()
	rec := ochre.EndpointRecord{
		ModelID:   testModelID,
		State:     ochre.StateReady,
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
	out := formatEndpointRecord(ochre.EndpointRecord{ModelID: testModelID, State: ochre.StateAbsent})
	require.Contains(t, out, "ABSENT")
	require.Equal(t, 2, strings.Count(strings.TrimSpace(out), "\n")+1)
}

// withEndpointService swaps endpointServiceFn for one returning svc, so a Run
// wrapper's real connect/validate/exit control flow runs without a daemon.
func withEndpointService(t *testing.T, svc ochre.EndpointService, connErr error) {
	t.Helper()
	orig := endpointServiceFn
	t.Cleanup(func() { endpointServiceFn = orig })
	endpointServiceFn = func() (ochre.EndpointService, func(), error) {
		if connErr != nil {
			return nil, nil, connErr
		}
		return svc, func() {}, nil
	}
}

func TestRunOchreEndpointEnsure_PrintsRecord(t *testing.T) {
	withEndpointService(t, &fakeEndpointService{
		ensure: ochre.EndpointRecord{ModelID: testModelID, State: ochre.StateStarting},
	}, nil)

	cmd := *ochreEndpointEnsureCmd
	require.NoError(t, cmd.Flags().Set("model-id", testModelID))

	var out string
	code := withOchreExitCapture(t, func() {
		out = captureStdout(t, func() { runOchreEndpointEnsure(&cmd, nil) })
	})
	require.Equal(t, -1, code)
	require.Contains(t, out, "STARTING")
}

func TestRunOchreEndpointEnsure_ConnectFailureExits1(t *testing.T) {
	withEndpointService(t, nil, errors.New("dial nats: connection refused"))

	cmd := *ochreEndpointEnsureCmd
	require.NoError(t, cmd.Flags().Set("model-id", testModelID))

	code := withOchreExitCapture(t, func() { runOchreEndpointEnsure(&cmd, nil) })
	require.Equal(t, 1, code)
}

func TestRunOchreEndpointEnsure_ServiceErrorExits1(t *testing.T) {
	withEndpointService(t, &fakeEndpointService{ensureErr: errors.New("ResourceNotFoundException")}, nil)

	cmd := *ochreEndpointEnsureCmd
	require.NoError(t, cmd.Flags().Set("model-id", "no.such.model"))

	code := withOchreExitCapture(t, func() { runOchreEndpointEnsure(&cmd, nil) })
	require.Equal(t, 1, code)
}

func TestRunOchreEndpointDescribe_PrintsRecord(t *testing.T) {
	withEndpointService(t, &fakeEndpointService{describes: []ochre.EndpointRecord{
		{ModelID: testModelID, State: ochre.StateReady, InstanceID: "i-abc"},
	}}, nil)

	cmd := *ochreEndpointDescribeCmd
	require.NoError(t, cmd.Flags().Set("model-id", testModelID))

	var out string
	code := withOchreExitCapture(t, func() {
		out = captureStdout(t, func() { runOchreEndpointDescribe(&cmd, nil) })
	})
	require.Equal(t, -1, code)
	require.Contains(t, out, "i-abc")
}

// TestRunOchreEndpointDescribe_DefaultAccountIsEmpty covers the regression
// guard: a bare (GlobalAccountID) describe, with no --account flag set, must
// still send an empty AccountID — resolveAccountID's own GlobalAccountID
// fallback, unchanged from before --account existed.
func TestRunOchreEndpointDescribe_DefaultAccountIsEmpty(t *testing.T) {
	svc := &fakeEndpointService{describes: []ochre.EndpointRecord{
		{ModelID: testModelID, State: ochre.StateReady},
	}}
	withEndpointService(t, svc, nil)

	cmd := *ochreEndpointDescribeCmd
	require.NoError(t, cmd.Flags().Set("model-id", testModelID))

	withOchreExitCapture(t, func() {
		captureStdout(t, func() { runOchreEndpointDescribe(&cmd, nil) })
	})
	require.Len(t, svc.describeInputs, 1)
	require.Empty(t, svc.describeInputs[0].AccountID)
}

// TestRunOchreEndpointDescribe_AccountFlagScopesLookup is Bug 2's describe
// seam: --account must reach DescribeEndpointInput.AccountID, which is how an
// operator sees a pinned, account-scoped endpoint the GlobalAccountID lookup
// would otherwise report ABSENT.
func TestRunOchreEndpointDescribe_AccountFlagScopesLookup(t *testing.T) {
	svc := &fakeEndpointService{describes: []ochre.EndpointRecord{
		{ModelID: testModelID, State: ochre.StateReady, AccountID: testAccountID, Pinned: true},
	}}
	withEndpointService(t, svc, nil)

	cmd := *ochreEndpointDescribeCmd
	require.NoError(t, cmd.Flags().Set("model-id", testModelID))
	require.NoError(t, cmd.Flags().Set("account", testAccountID))

	var out string
	withOchreExitCapture(t, func() {
		out = captureStdout(t, func() { runOchreEndpointDescribe(&cmd, nil) })
	})
	require.Len(t, svc.describeInputs, 1)
	require.Equal(t, testAccountID, svc.describeInputs[0].AccountID)
	require.Contains(t, out, testAccountID)
	require.Contains(t, out, "Pinned")
}

func TestRunOchreEndpointDescribe_ErrorExits1(t *testing.T) {
	withEndpointService(t, &fakeEndpointService{describeErr: errors.New("nats: timeout")}, nil)

	cmd := *ochreEndpointDescribeCmd
	require.NoError(t, cmd.Flags().Set("model-id", testModelID))

	code := withOchreExitCapture(t, func() { runOchreEndpointDescribe(&cmd, nil) })
	require.Equal(t, 1, code)
}

func TestRunOchreEndpointList_PrintsTable(t *testing.T) {
	withEndpointService(t, &fakeEndpointService{list: []ochre.EndpointRecord{
		{ModelID: testModelID, State: ochre.StateReady},
	}}, nil)

	var out string
	code := withOchreExitCapture(t, func() {
		out = captureStdout(t, func() { runOchreEndpointList(ochreEndpointListCmd, nil) })
	})
	require.Equal(t, -1, code)
	require.Contains(t, out, testModelID)
}

func TestRunOchreEndpointList_ConnectFailureExits1(t *testing.T) {
	withEndpointService(t, nil, errors.New("dial nats: connection refused"))

	code := withOchreExitCapture(t, func() { runOchreEndpointList(ochreEndpointListCmd, nil) })
	require.Equal(t, 1, code)
}

func TestRunOchreEndpointDelete_ReportsTeardown(t *testing.T) {
	svc := &fakeEndpointService{deleteRemoved: true}
	withEndpointService(t, svc, nil)

	cmd := *ochreEndpointDeleteCmd
	require.NoError(t, cmd.Flags().Set("model-id", testModelID))

	var out string
	code := withOchreExitCapture(t, func() {
		out = captureStdout(t, func() { runOchreEndpointDelete(&cmd, nil) })
	})
	require.Equal(t, -1, code)
	require.Equal(t, 1, svc.deleteCalls)
	require.Contains(t, out, "torn down")
}

// TestRunOchreEndpointDelete_NoRecordDoesNotClaimTeardown is 6z9vg's honesty
// guard: a delete that removed nothing must not print "torn down", and must
// point the operator at --account so a pinned, account-scoped record they can
// see in 'list' is reachable.
func TestRunOchreEndpointDelete_NoRecordDoesNotClaimTeardown(t *testing.T) {
	svc := &fakeEndpointService{deleteRemoved: false}
	withEndpointService(t, svc, nil)

	cmd := *ochreEndpointDeleteCmd
	require.NoError(t, cmd.Flags().Set("model-id", testModelID))

	var out string
	code := withOchreExitCapture(t, func() {
		out = captureStdout(t, func() { runOchreEndpointDelete(&cmd, nil) })
	})
	require.Equal(t, -1, code)
	require.NotContains(t, out, "torn down")
	require.Contains(t, out, "--account")
}

// TestRunOchreEndpointDelete_AccountFlagReachesInput is 6z9vg's core seam: the
// --account flag must reach DeleteEndpointInput.AccountID, which is how an
// operator tears down a pinned endpoint the bare GlobalAccountID key misses.
func TestRunOchreEndpointDelete_AccountFlagReachesInput(t *testing.T) {
	svc := &fakeEndpointService{deleteRemoved: true}
	withEndpointService(t, svc, nil)

	cmd := *ochreEndpointDeleteCmd
	require.NoError(t, cmd.Flags().Set("model-id", testModelID))
	require.NoError(t, cmd.Flags().Set("account", testAccountID))

	code := withOchreExitCapture(t, func() {
		_ = captureStdout(t, func() { runOchreEndpointDelete(&cmd, nil) })
	})
	require.Equal(t, -1, code)
	require.Len(t, svc.deleteInputs, 1)
	require.Equal(t, testAccountID, svc.deleteInputs[0].AccountID)
}

func TestRunOchreEndpointDelete_ErrorExits1(t *testing.T) {
	withEndpointService(t, &fakeEndpointService{deleteErr: errors.New("ModelNotReadyException")}, nil)

	cmd := *ochreEndpointDeleteCmd
	require.NoError(t, cmd.Flags().Set("model-id", testModelID))

	code := withOchreExitCapture(t, func() { runOchreEndpointDelete(&cmd, nil) })
	require.Equal(t, 1, code)
}

// The reclaim inputs are what an operator needs to answer "why was this
// endpoint reaped" or "why was it not", so a READY record must show them.
func TestFormatEndpointRecord_ShowsReclaimInputs(t *testing.T) {
	ready := time.Now().UTC().Add(-30 * time.Minute)
	out := formatEndpointRecord(ochre.EndpointRecord{
		ModelID:      testModelID,
		State:        ochre.StateReady,
		ReadyAt:      ready,
		LastActiveAt: ready.Add(20 * time.Minute),
		InFlight:     3,
	})

	require.Contains(t, out, "In flight:")
	require.Contains(t, out, "3")
	require.Contains(t, out, "Last active:")
	require.Contains(t, out, "Idle for:")
}

// An endpoint quiet since launch is idle since launch, not since the zero
// time: the fallback is what keeps the reported figure meaningful.
func TestFormatEndpointRecord_NeverActiveFallsBackToReadyAt(t *testing.T) {
	out := formatEndpointRecord(ochre.EndpointRecord{
		ModelID: testModelID,
		State:   ochre.StateReady,
		ReadyAt: time.Now().UTC().Add(-90 * time.Second),
	})

	require.Contains(t, out, "Idle for:")
	require.NotContains(t, out, "0001-01-01", "an unsampled record must not report the zero time")
	require.NotContains(t, out, "Scrape failures", "a healthy endpoint must not carry a failure row")
}

func TestFormatEndpointRecord_SurfacesPinnedAndScrapeFailures(t *testing.T) {
	out := formatEndpointRecord(ochre.EndpointRecord{
		ModelID:        testModelID,
		State:          ochre.StateReady,
		ReadyAt:        time.Now().UTC().Add(-time.Hour),
		Pinned:         true,
		ScrapeFailures: 2,
	})

	require.Contains(t, out, "Pinned:")
	require.Contains(t, out, "Scrape failures:")
	require.Contains(t, out, "2")
}

// Only READY endpoints are swept, so reclaim rows on any other state would be
// reporting a decision nothing is making.
func TestFormatEndpointRecord_NoReclaimRowsWhenNotReady(t *testing.T) {
	out := formatEndpointRecord(ochre.EndpointRecord{
		ModelID: testModelID,
		State:   ochre.StateStarting,
	})

	require.NotContains(t, out, "In flight")
	require.NotContains(t, out, "Idle for")
}
