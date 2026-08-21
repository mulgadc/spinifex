//test:in-package — reuses startTestNATSServer, the package's unexported NATS
//fixture that every other gateway instance test is built on.

package gateway_ec2_instance

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	"github.com/nats-io/nats.go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// serveMonitoring answers the daemon subject with the tier the request asked
// for, and records the subject it was reached on.
func serveMonitoring(t *testing.T, nc *nats.Conn, subject, state string, ids ...string) *string {
	t.Helper()
	seen := new(string)
	monitorings := make([]*ec2.InstanceMonitoring, 0, len(ids))
	for _, id := range ids {
		monitorings = append(monitorings, &ec2.InstanceMonitoring{
			InstanceId: aws.String(id),
			Monitoring: &ec2.Monitoring{State: aws.String(state)},
		})
	}
	_, err := nc.Subscribe(subject, func(msg *nats.Msg) {
		*seen = msg.Subject
		body, err := json.Marshal(&ec2.MonitorInstancesOutput{InstanceMonitorings: monitorings})
		require.NoError(t, err)
		require.NoError(t, msg.Respond(body))
	})
	require.NoError(t, err)
	return seen
}

func TestMonitorInstances_Success(t *testing.T) {
	_, nc := startTestNATSServer(t)
	seen := serveMonitoring(t, nc, "ec2.MonitorInstances", ec2.MonitoringStateEnabled, "i-0123456789abcdef0")

	out, err := MonitorInstances(context.Background(), &ec2.MonitorInstancesInput{
		InstanceIds: []*string{aws.String("i-0123456789abcdef0")},
	}, nc, "123456789012")

	require.NoError(t, err)
	require.Len(t, out.InstanceMonitorings, 1)
	assert.Equal(t, ec2.MonitoringStateEnabled, aws.StringValue(out.InstanceMonitorings[0].Monitoring.State))
	assert.Equal(t, "ec2.MonitorInstances", *seen)
}

func TestUnmonitorInstances_Success(t *testing.T) {
	_, nc := startTestNATSServer(t)
	seen := serveMonitoring(t, nc, "ec2.UnmonitorInstances", ec2.MonitoringStateDisabled, "i-0123456789abcdef0")

	out, err := UnmonitorInstances(context.Background(), &ec2.UnmonitorInstancesInput{
		InstanceIds: []*string{aws.String("i-0123456789abcdef0")},
	}, nc, "123456789012")

	require.NoError(t, err)
	require.Len(t, out.InstanceMonitorings, 1)
	assert.Equal(t, ec2.MonitoringStateDisabled, aws.StringValue(out.InstanceMonitorings[0].Monitoring.State))
	assert.Equal(t, "ec2.UnmonitorInstances", *seen)
}

// The two actions must not share a subject, or disabling monitoring would
// enable it.
func TestMonitorInstances_DistinctSubjects(t *testing.T) {
	_, nc := startTestNATSServer(t)
	serveMonitoring(t, nc, "ec2.MonitorInstances", ec2.MonitoringStateEnabled, "i-0123456789abcdef0")

	_, err := UnmonitorInstances(context.Background(), &ec2.UnmonitorInstancesInput{
		InstanceIds: []*string{aws.String("i-0123456789abcdef0")},
	}, nc, "123456789012")
	require.Error(t, err, "unmonitor must not be served by the monitor subscriber")
}

func TestMonitorInstances_InvalidInput(t *testing.T) {
	_, nc := startTestNATSServer(t)

	tests := []struct {
		name string
		ids  []*string
		want string
	}{
		{"empty list", nil, awserrors.ErrorMissingParameter},
		{"nil id", []*string{nil}, awserrors.ErrorInvalidInstanceIDMalformed},
		{"missing i- prefix", []*string{aws.String("0123456789abcdef0")}, awserrors.ErrorInvalidInstanceIDMalformed},
		// One bad id in an otherwise valid list must reject the whole call
		// rather than silently apply the tier to the good ones.
		{"one bad id", []*string{aws.String("i-0123456789abcdef0"), aws.String("vol-1")}, awserrors.ErrorInvalidInstanceIDMalformed},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := MonitorInstances(context.Background(), &ec2.MonitorInstancesInput{InstanceIds: tt.ids}, nc, "123456789012")
			require.Error(t, err)
			assert.Equal(t, tt.want, err.Error())

			_, err = UnmonitorInstances(context.Background(), &ec2.UnmonitorInstancesInput{InstanceIds: tt.ids}, nc, "123456789012")
			require.Error(t, err)
			assert.Equal(t, tt.want, err.Error())
		})
	}
}

func TestMonitorInstances_NilInput(t *testing.T) {
	_, nc := startTestNATSServer(t)

	_, err := MonitorInstances(context.Background(), nil, nc, "123456789012")
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorInvalidParameterValue, err.Error())

	_, err = UnmonitorInstances(context.Background(), nil, nc, "123456789012")
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorInvalidParameterValue, err.Error())
}
