//go:build integration

package integration

import (
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/stretchr/testify/require"
)

// TestSerialConsoleAccess ports tests/e2e/single/discovery_test.go's
// runSerialConsoleAccess: it flips the account-wide serial-console-access
// setting on then off, checking every transition with both the
// action-returned bool and a follow-up get-status round-trip, against the
// real AccountSettingsServiceImpl DaemonLite wires up (the same production
// code and NATS JetStream KV persistence a live daemon uses).
func TestSerialConsoleAccess(t *testing.T) {
	t.Parallel()

	gw := StartGateway(t)
	StartDaemonLite(t, gw)
	ec2Cli := gw.EC2Client(t)

	statusOut, err := ec2Cli.GetSerialConsoleAccessStatus(&ec2.GetSerialConsoleAccessStatusInput{})
	require.NoError(t, err, "get-serial-console-access-status (default)")
	require.False(t, aws.BoolValue(statusOut.SerialConsoleAccessEnabled), "expected serial console default disabled")

	enableOut, err := ec2Cli.EnableSerialConsoleAccess(&ec2.EnableSerialConsoleAccessInput{})
	require.NoError(t, err, "enable-serial-console-access")
	require.True(t, aws.BoolValue(enableOut.SerialConsoleAccessEnabled), "enable: action returned enabled=false")

	statusOut, err = ec2Cli.GetSerialConsoleAccessStatus(&ec2.GetSerialConsoleAccessStatusInput{})
	require.NoError(t, err, "get-serial-console-access-status (after enable)")
	require.True(t, aws.BoolValue(statusOut.SerialConsoleAccessEnabled), "enable: subsequent get-status returned disabled")

	disableOut, err := ec2Cli.DisableSerialConsoleAccess(&ec2.DisableSerialConsoleAccessInput{})
	require.NoError(t, err, "disable-serial-console-access")
	require.False(t, aws.BoolValue(disableOut.SerialConsoleAccessEnabled), "disable: action returned enabled=true")

	statusOut, err = ec2Cli.GetSerialConsoleAccessStatus(&ec2.GetSerialConsoleAccessStatusInput{})
	require.NoError(t, err, "get-serial-console-access-status (after disable)")
	require.False(t, aws.BoolValue(statusOut.SerialConsoleAccessEnabled), "disable: subsequent get-status returned enabled")
}
