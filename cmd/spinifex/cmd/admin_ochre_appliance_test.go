// Exercises unexported ochre appliance CLI command internals with no
// exported surface to drive them through.
//
//test:in-package
package cmd

import (
	"context"
	"errors"
	"testing"

	"github.com/mulgadc/spinifex/spinifex/testutil"
	"github.com/stretchr/testify/require"
)

// TestRunApplianceTeardownGuarded_RefusesWithoutConfirm proves the guard
// refuses before ever invoking teardown -- a missing --confirm must never
// reach a NATS call, let alone destroy the appliance.
func TestRunApplianceTeardownGuarded_RefusesWithoutConfirm(t *testing.T) {
	called := false
	teardown := func(context.Context, bool) error {
		called = true
		return nil
	}

	_, err := runApplianceTeardownGuarded(context.Background(), false, false, teardown)
	require.Error(t, err)
	require.ErrorContains(t, err, "--confirm")
	require.False(t, called, "teardown must not run without --confirm")
}

// TestRunApplianceTeardownGuarded_ConfirmedInvokesTeardown proves --confirm
// drives the teardown call through and reports success.
func TestRunApplianceTeardownGuarded_ConfirmedInvokesTeardown(t *testing.T) {
	called := false
	teardown := func(context.Context, bool) error {
		called = true
		return nil
	}

	msg, err := runApplianceTeardownGuarded(context.Background(), true, false, teardown)
	require.NoError(t, err)
	require.True(t, called)
	require.Contains(t, msg, "torn down")
}

// TestRunApplianceTeardownGuarded_TeardownErrorSurfaces proves a teardown
// failure (e.g. the daemon reporting no appliance up) surfaces to the caller
// rather than being swallowed behind a generic success message.
func TestRunApplianceTeardownGuarded_TeardownErrorSurfaces(t *testing.T) {
	teardown := func(context.Context, bool) error {
		return errors.New("ochrevector: platform appliance is not enabled or not up on this node")
	}

	_, err := runApplianceTeardownGuarded(context.Background(), true, false, teardown)
	require.Error(t, err)
	require.ErrorContains(t, err, "not enabled or not up")
}

// TestRunApplianceTeardownGuarded_PurgeMetadataThreadsThrough proves the
// --purge-metadata flag reaches the teardown call and is reflected in the
// success message, so an operator can tell which mode ran.
func TestRunApplianceTeardownGuarded_PurgeMetadataThreadsThrough(t *testing.T) {
	var gotPurge bool
	teardown := func(_ context.Context, purgeMetadata bool) error {
		gotPurge = purgeMetadata
		return nil
	}

	msg, err := runApplianceTeardownGuarded(context.Background(), true, true, teardown)
	require.NoError(t, err)
	require.True(t, gotPurge, "purgeMetadata must reach the teardown call")
	require.Contains(t, msg, "metadata purged")
}

// TestApplianceTeardownFn_ThreadsPurgeMetadataOverNATS drives the real
// (unstubbed) applianceTeardownFn through runApplianceTeardownGuarded, so the
// actual NATSRequest call -- including the PurgeMetadata field on the wire
// request -- executes rather than a test double standing in for it. No
// daemon is subscribed to the teardown subject in this test, so the call
// fails fast; the point is that it's reached and built with PurgeMetadata set.
func TestApplianceTeardownFn_ThreadsPurgeMetadataOverNATS(t *testing.T) {
	_, nc, _ := testutil.StartTestJetStream(t)
	stubConnect(t, fakeClusterConfig(), nc, nil)

	_, err := runApplianceTeardownGuarded(context.Background(), true, true, applianceTeardownFn)
	require.Error(t, err, "no daemon is subscribed to the teardown subject in this test")
}

// withApplianceTeardown swaps applianceTeardownFn for one that never dials
// NATS, so the Run wrapper's real flag/exit control flow is exercised
// without a live daemon.
func withApplianceTeardown(t *testing.T, fn func(context.Context, bool) error) {
	t.Helper()
	orig := applianceTeardownFn
	t.Cleanup(func() { applianceTeardownFn = orig })
	applianceTeardownFn = fn
}

func TestRunOchreApplianceTeardown_MissingConfirmExits1(t *testing.T) {
	withApplianceTeardown(t, func(context.Context, bool) error {
		t.Fatal("teardown must not be called without --confirm")
		return nil
	})

	cmd := *ochreApplianceTeardownCmd
	code := withOchreExitCapture(t, func() { runOchreApplianceTeardown(&cmd, nil) })
	require.Equal(t, 1, code)
}

func TestRunOchreApplianceTeardown_ConfirmedPrintsSuccess(t *testing.T) {
	withApplianceTeardown(t, func(context.Context, bool) error { return nil })

	cmd := *ochreApplianceTeardownCmd
	require.NoError(t, cmd.Flags().Set("confirm", "true"))

	var out string
	code := withOchreExitCapture(t, func() {
		out = captureStdout(t, func() { runOchreApplianceTeardown(&cmd, nil) })
	})
	require.Equal(t, -1, code)
	require.Contains(t, out, "torn down")
}

func TestRunOchreApplianceTeardown_TeardownErrorExits1(t *testing.T) {
	withApplianceTeardown(t, func(context.Context, bool) error {
		return errors.New("ochrevector: platform appliance is not enabled or not up on this node")
	})

	cmd := *ochreApplianceTeardownCmd
	require.NoError(t, cmd.Flags().Set("confirm", "true"))

	code := withOchreExitCapture(t, func() { runOchreApplianceTeardown(&cmd, nil) })
	require.Equal(t, 1, code)
}

// TestRunOchreApplianceTeardown_PurgeMetadataFlagReachesTeardownFn proves
// the --purge-metadata flag is read and passed through the Run wrapper.
func TestRunOchreApplianceTeardown_PurgeMetadataFlagReachesTeardownFn(t *testing.T) {
	var gotPurge bool
	withApplianceTeardown(t, func(_ context.Context, purgeMetadata bool) error {
		gotPurge = purgeMetadata
		return nil
	})

	cmd := *ochreApplianceTeardownCmd
	require.NoError(t, cmd.Flags().Set("confirm", "true"))
	require.NoError(t, cmd.Flags().Set("purge-metadata", "true"))

	code := withOchreExitCapture(t, func() { runOchreApplianceTeardown(&cmd, nil) })
	require.Equal(t, -1, code)
	require.True(t, gotPurge, "--purge-metadata must reach applianceTeardownFn")
}
