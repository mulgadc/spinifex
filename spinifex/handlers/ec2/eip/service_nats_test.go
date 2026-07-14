package handlers_ec2_eip

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/spinifex/utils"
	"github.com/nats-io/nats.go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func handleNATSMsg[In any, Out any](msg *nats.Msg, fn func(context.Context, *In, string) (*Out, error)) {
	var input In
	if err := json.Unmarshal(msg.Data, &input); err != nil {
		_ = msg.Respond(utils.GenerateErrorPayload("ValidationError"))
		return
	}
	accountID := msg.Header.Get(utils.AccountIDHeader)
	result, err := fn(context.Background(), &input, accountID)
	if err != nil {
		_ = msg.Respond(utils.GenerateErrorPayload(err.Error()))
		return
	}
	data, _ := json.Marshal(result)
	_ = msg.Respond(data)
}

func setupNATSEIPServiceTest(t *testing.T) (EIPService, *EIPServiceImpl) {
	t.Helper()

	backend, _ := setupTestEIP(t)

	nc, err := nats.Connect(backend.natsConn.ConnectedUrl())
	require.NoError(t, err)
	t.Cleanup(func() { nc.Close() })

	topics := map[string]func(*nats.Msg){
		"ec2.AllocateAddress":     func(msg *nats.Msg) { handleNATSMsg(msg, backend.AllocateAddress) },
		"ec2.ReleaseAddress":      func(msg *nats.Msg) { handleNATSMsg(msg, backend.ReleaseAddress) },
		"ec2.AssociateAddress":    func(msg *nats.Msg) { handleNATSMsg(msg, backend.AssociateAddress) },
		"ec2.DisassociateAddress": func(msg *nats.Msg) { handleNATSMsg(msg, backend.DisassociateAddress) },
		"ec2.DescribeAddresses":   func(msg *nats.Msg) { handleNATSMsg(msg, backend.DescribeAddresses) },
	}

	for topic, handler := range topics {
		sub, err := nc.Subscribe(topic, handler)
		require.NoError(t, err)
		t.Cleanup(func() { _ = sub.Unsubscribe() })
	}

	client := NewNATSEIPService(nc)
	return client, backend
}

func TestNATSEIPService_AllocateAddress(t *testing.T) {
	client, _ := setupNATSEIPServiceTest(t)

	out, err := client.AllocateAddress(context.Background(), &ec2.AllocateAddressInput{}, testAccountID)
	require.NoError(t, err)
	require.NotNil(t, out)
	assert.NotEmpty(t, *out.AllocationId)
	assert.NotEmpty(t, *out.PublicIp)
	assert.Equal(t, "vpc", *out.Domain)
}

func TestNATSEIPService_DescribeAddresses(t *testing.T) {
	client, _ := setupNATSEIPServiceTest(t)

	_, err := client.AllocateAddress(context.Background(), &ec2.AllocateAddressInput{}, testAccountID)
	require.NoError(t, err)

	out, err := client.DescribeAddresses(context.Background(), &ec2.DescribeAddressesInput{}, testAccountID)
	require.NoError(t, err)
	assert.GreaterOrEqual(t, len(out.Addresses), 1)
}

func TestNATSEIPService_ReleaseAddress(t *testing.T) {
	client, _ := setupNATSEIPServiceTest(t)

	allocOut, err := client.AllocateAddress(context.Background(), &ec2.AllocateAddressInput{}, testAccountID)
	require.NoError(t, err)

	_, err = client.ReleaseAddress(context.Background(), &ec2.ReleaseAddressInput{
		AllocationId: allocOut.AllocationId,
	}, testAccountID)
	require.NoError(t, err)
}

func TestNATSEIPService_AssociateAddress_MissingParams(t *testing.T) {
	client, _ := setupNATSEIPServiceTest(t)

	// Missing AllocationId should return error through NATS
	_, err := client.AssociateAddress(context.Background(), &ec2.AssociateAddressInput{}, testAccountID)
	assert.Error(t, err)
}

func TestNATSEIPService_DisassociateAddress_MissingParams(t *testing.T) {
	client, _ := setupNATSEIPServiceTest(t)

	// Missing AssociationId should return error through NATS
	_, err := client.DisassociateAddress(context.Background(), &ec2.DisassociateAddressInput{}, testAccountID)
	assert.Error(t, err)
}

func TestNATSEIPService_AllocateAndDescribeRoundTrip(t *testing.T) {
	client, _ := setupNATSEIPServiceTest(t)

	// Allocate two EIPs
	out1, err := client.AllocateAddress(context.Background(), &ec2.AllocateAddressInput{}, testAccountID)
	require.NoError(t, err)
	out2, err := client.AllocateAddress(context.Background(), &ec2.AllocateAddressInput{}, testAccountID)
	require.NoError(t, err)

	// Describe should show both
	desc, err := client.DescribeAddresses(context.Background(), &ec2.DescribeAddressesInput{}, testAccountID)
	require.NoError(t, err)
	assert.Len(t, desc.Addresses, 2)

	// Release one and verify count drops
	_, err = client.ReleaseAddress(context.Background(), &ec2.ReleaseAddressInput{AllocationId: out1.AllocationId}, testAccountID)
	require.NoError(t, err)

	desc, err = client.DescribeAddresses(context.Background(), &ec2.DescribeAddressesInput{}, testAccountID)
	require.NoError(t, err)
	assert.Len(t, desc.Addresses, 1)
	assert.Equal(t, *out2.AllocationId, *desc.Addresses[0].AllocationId)
}
