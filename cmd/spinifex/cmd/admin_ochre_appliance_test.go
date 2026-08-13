// Exercises unexported ochre appliance CLI command internals with no
// exported surface to drive them through.
//
//test:in-package
package cmd

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestRunApplianceTeardownGuarded_RefusesWithoutConfirm proves the guard
// refuses before ever invoking teardown -- a missing --confirm must never
// reach a NATS call, let alone destroy the appliance.
func TestRunApplianceTeardownGuarded_RefusesWithoutConfirm(t *testing.T) {
	called := false
	teardown := func(context.Context) error {
		called = true
		return nil
	}

	_, err := runApplianceTeardownGuarded(context.Background(), false, teardown)
	require.Error(t, err)
	require.ErrorContains(t, err, "--confirm")
	require.False(t, called, "teardown must not run without --confirm")
}

// TestRunApplianceTeardownGuarded_ConfirmedInvokesTeardown proves --confirm
// drives the teardown call through and reports success.
func TestRunApplianceTeardownGuarded_ConfirmedInvokesTeardown(t *testing.T) {
	called := false
	teardown := func(context.Context) error {
		called = true
		return nil
	}

	msg, err := runApplianceTeardownGuarded(context.Background(), true, teardown)
	require.NoError(t, err)
	require.True(t, called)
	require.Contains(t, msg, "torn down")
}

// TestRunApplianceTeardownGuarded_TeardownErrorSurfaces proves a teardown
// failure (e.g. the daemon reporting no appliance up) surfaces to the caller
// rather than being swallowed behind a generic success message.
func TestRunApplianceTeardownGuarded_TeardownErrorSurfaces(t *testing.T) {
	teardown := func(context.Context) error {
		return errors.New("ochrevector: platform appliance is not enabled or not up on this node")
	}

	_, err := runApplianceTeardownGuarded(context.Background(), true, teardown)
	require.Error(t, err)
	require.ErrorContains(t, err, "not enabled or not up")
}

// withApplianceTeardown swaps applianceTeardownFn for one that never dials
// NATS, so the Run wrapper's real flag/exit control flow is exercised
// without a live daemon.
func withApplianceTeardown(t *testing.T, fn func(context.Context) error) {
	t.Helper()
	orig := applianceTeardownFn
	t.Cleanup(func() { applianceTeardownFn = orig })
	applianceTeardownFn = fn
}

func TestRunOchreApplianceTeardown_MissingConfirmExits1(t *testing.T) {
	withApplianceTeardown(t, func(context.Context) error {
		t.Fatal("teardown must not be called without --confirm")
		return nil
	})

	cmd := *ochreApplianceTeardownCmd
	code := withOchreExitCapture(t, func() { runOchreApplianceTeardown(&cmd, nil) })
	require.Equal(t, 1, code)
}

func TestRunOchreApplianceTeardown_ConfirmedPrintsSuccess(t *testing.T) {
	withApplianceTeardown(t, func(context.Context) error { return nil })

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
	withApplianceTeardown(t, func(context.Context) error {
		return errors.New("ochrevector: platform appliance is not enabled or not up on this node")
	})

	cmd := *ochreApplianceTeardownCmd
	require.NoError(t, cmd.Flags().Set("confirm", "true"))

	code := withOchreExitCapture(t, func() { runOchreApplianceTeardown(&cmd, nil) })
	require.Equal(t, 1, code)
}
