//test:in-package — drives the public DeleteVolume action through EC2_Request
// with the gateway's unexported test helpers.

package gateway

import (
	"context"
	ec2awsapi "github.com/mulgadc/spinifex/spinifex/domains/ec2/awsapi"
	"github.com/mulgadc/spinifex/spinifex/runtime/roles/awsgw/credverify"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mulgadc/spinifex/internal/testkit"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/mulgadc/spinifex/spinifex/foundation/messaging/nats"
	"github.com/nats-io/nats.go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A configured node that never answers leaves the instance view incomplete, so
// a guest there could still map the volume. The public action must refuse with
// a retryable ServiceUnavailable and never reach the volume service.
func TestDeleteVolumeAction_MissingConfiguredNodeIsRetryableAndDeletesNothing(t *testing.T) {
	t.Parallel()
	_, nc := testutil.StartTestNATS(t)
	answer := func(subject, node string) {
		sub, err := nc.Subscribe(subject, func(msg *nats.Msg) {
			out := nats.NewMsg(msg.Reply)
			out.Data = []byte(`{}`)
			if node != "" {
				out.Header.Set(natsmsg.NodeIDHeader, node)
			}
			_ = msg.RespondMsg(out)
		})
		require.NoError(t, err)
		t.Cleanup(func() { _ = sub.Unsubscribe() })
	}
	answer("ec2.DescribeInstances", "node-1")
	answer("ec2.DescribeStoppedInstances", "")
	answer("ec2.DescribeTerminatedInstances", "")
	var volumeCalls atomic.Int32
	for _, subject := range []string{"ec2.DeleteVolume", "ec2.ForceDetachVolume"} {
		sub, err := nc.Subscribe(subject, func(msg *nats.Msg) {
			volumeCalls.Add(1)
			_ = msg.Respond([]byte(`{}`))
		})
		require.NoError(t, err)
		t.Cleanup(func() { _ = sub.Unsubscribe() })
	}
	require.NoError(t, nc.Flush())

	gw := &GatewayConfig{
		DisableLogging: true,
		NATSConn:       nc,
		IAMService:     allowAllIAMService(),
		Credentials:    credverify.New(allowAllIAMService(), nil),
	}
	withEC2(gw, func(d *ec2awsapi.Deps) { d.NodeIDs = []string{"node-1", "node-2"} })
	req := setupEC2Request("Action=DeleteVolume&VolumeId=vol-0123456789abcdef0", "123456789012")
	ctx, cancel := context.WithTimeout(req.Context(), 30*time.Second)
	defer cancel()
	err := gw.EC2_Request(httptest.NewRecorder(), req.WithContext(ctx))

	require.Error(t, err)
	code, ok := awserrors.ResolveErrorCode(err)
	require.True(t, ok)
	assert.Equal(t, awserrors.ErrorServiceUnavailable, code)
	_, retryable := awserrors.ResolveRetryAfter(err)
	assert.True(t, retryable, "the refusal must tell clients when to retry")
	assert.Zero(t, volumeCalls.Load(), "the volume must not be detached or deleted")
}
