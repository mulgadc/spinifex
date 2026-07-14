package gateway_ec2_spotinstance

import (
	"context"
	"encoding/json"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/spinifex/testutil"
	"github.com/mulgadc/spinifex/spinifex/types"
	"github.com/mulgadc/spinifex/spinifex/utils"
	"github.com/nats-io/nats.go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const lineageTestAccount = "111122223333"

// stampSpotLineage targets each instance's owner subject (ec2.cmd.{id}) with the
// SetSpotLineage command carrying the SIR id and the account header.
func TestStampSpotLineage_TargetsOwnerWithSIRAndAccount(t *testing.T) {
	_, nc := testutil.StartTestNATS(t)

	got := make(chan *nats.Msg, 1)
	sub, err := nc.Subscribe("ec2.cmd.i-owner", func(m *nats.Msg) {
		got <- m
		_ = m.Respond([]byte(`{}`))
	})
	require.NoError(t, err)
	defer func() { _ = sub.Unsubscribe() }()

	requests := []*ec2.SpotInstanceRequest{{
		InstanceId:            aws.String("i-owner"),
		SpotInstanceRequestId: aws.String("sir-owner-1"),
	}}
	stampSpotLineage(context.Background(), nc, requests, lineageTestAccount)

	select {
	case m := <-got:
		assert.Equal(t, lineageTestAccount, m.Header.Get(utils.AccountIDHeader))
		var cmd types.EC2InstanceCommand
		require.NoError(t, json.Unmarshal(m.Data, &cmd))
		assert.Equal(t, "i-owner", cmd.ID)
		assert.True(t, cmd.Attributes.SetSpotLineage)
		require.NotNil(t, cmd.SpotLineageData)
		assert.Equal(t, "sir-owner-1", cmd.SpotLineageData.SpotInstanceRequestId)
	case <-time.After(2 * time.Second):
		t.Fatal("owner never received the spot lineage command")
	}
}

// The write-back retries past nats.ErrNoResponders until the owner subscribes,
// mirroring the daemon subscribing ec2.cmd.{id} only after the launch responds.
func TestSendSpotLineageCommand_RetriesPastNoResponders(t *testing.T) {
	_, nc := testutil.StartTestNATS(t)

	var received atomic.Int32
	go func() {
		time.Sleep(250 * time.Millisecond)
		sub, err := nc.Subscribe("ec2.cmd.i-late", func(m *nats.Msg) {
			received.Add(1)
			_ = m.Respond([]byte(`{}`))
		})
		if err == nil {
			t.Cleanup(func() { _ = sub.Unsubscribe() })
		}
	}()

	err := sendSpotLineageCommand(context.Background(), nc, "i-late", "sir-late-1", lineageTestAccount)
	require.NoError(t, err)
	assert.Equal(t, int32(1), received.Load())
}

// With no owner ever subscribing, the write-back exhausts its retries and
// returns the transport error rather than blocking forever.
func TestSendSpotLineageCommand_ReturnsErrorWhenNoOwner(t *testing.T) {
	_, nc := testutil.StartTestNATS(t)

	err := sendSpotLineageCommand(context.Background(), nc, "i-absent", "sir-absent-1", lineageTestAccount)
	require.Error(t, err)
}

// Requests with no mapped instance (no VM launched) are skipped — no command is sent.
func TestStampSpotLineage_SkipsRequestsWithNoInstance(t *testing.T) {
	_, nc := testutil.StartTestNATS(t)

	var received atomic.Int32
	sub, err := nc.Subscribe("ec2.cmd.>", func(m *nats.Msg) {
		received.Add(1)
		_ = m.Respond([]byte(`{}`))
	})
	require.NoError(t, err)
	defer func() { _ = sub.Unsubscribe() }()

	requests := []*ec2.SpotInstanceRequest{
		nil,
		{SpotInstanceRequestId: aws.String("sir-no-instance")},
		{InstanceId: aws.String("i-no-sir")},
	}
	stampSpotLineage(context.Background(), nc, requests, lineageTestAccount)

	assert.Equal(t, int32(0), received.Load())
}
