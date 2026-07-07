package handlers_ec2_igw

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/spinifex/utils"
	"github.com/nats-io/nats.go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const testAccountID = "123456789012"

// setupNATSIGWServiceTest creates a NATSIGWService client connected to an
// IGWServiceImpl backend via NATS.
func setupNATSIGWServiceTest(t *testing.T) (IGWService, *IGWServiceImpl) {
	t.Helper()

	backend, nc := setupTestIGWService(t)

	topics := map[string]func(*nats.Msg){
		"ec2.CreateInternetGateway":    func(msg *nats.Msg) { handleNATSMsg(msg, backend.CreateInternetGateway) },
		"ec2.DeleteInternetGateway":    func(msg *nats.Msg) { handleNATSMsg(msg, backend.DeleteInternetGateway) },
		"ec2.DescribeInternetGateways": func(msg *nats.Msg) { handleNATSMsg(msg, backend.DescribeInternetGateways) },
		"ec2.AttachInternetGateway":    func(msg *nats.Msg) { handleNATSMsg(msg, backend.AttachInternetGateway) },
		"ec2.DetachInternetGateway":    func(msg *nats.Msg) { handleNATSMsg(msg, backend.DetachInternetGateway) },
	}

	for topic, handler := range topics {
		sub, err := nc.Subscribe(topic, handler)
		require.NoError(t, err)
		t.Cleanup(func() { _ = sub.Unsubscribe() })
	}

	client := NewNATSIGWService(nc)
	return client, backend
}

func handleNATSMsg[In any, Out any](msg *nats.Msg, fn func(context.Context, *In, string) (*Out, error)) {
	var input In
	if err := json.Unmarshal(msg.Data, &input); err != nil {
		_ = msg.Respond([]byte(`{"error":"unmarshal"}`))
		return
	}
	accountID := msg.Header.Get(utils.AccountIDHeader)
	result, err := fn(context.Background(), &input, accountID)
	if err != nil {
		errResp, _ := json.Marshal(map[string]string{"error": err.Error()})
		_ = msg.Respond(errResp)
		return
	}
	data, _ := json.Marshal(result)
	_ = msg.Respond(data)
}

func TestNATSIGWService_CreateInternetGateway(t *testing.T) {
	client, _ := setupNATSIGWServiceTest(t)

	out, err := client.CreateInternetGateway(context.Background(), &ec2.CreateInternetGatewayInput{}, testAccountID)
	require.NoError(t, err)
	require.NotNil(t, out.InternetGateway)
	assert.NotEmpty(t, *out.InternetGateway.InternetGatewayId)
}

func TestNATSIGWService_DescribeInternetGateways(t *testing.T) {
	client, _ := setupNATSIGWServiceTest(t)

	_, err := client.CreateInternetGateway(context.Background(), &ec2.CreateInternetGatewayInput{}, testAccountID)
	require.NoError(t, err)

	out, err := client.DescribeInternetGateways(context.Background(), &ec2.DescribeInternetGatewaysInput{}, testAccountID)
	require.NoError(t, err)
	assert.GreaterOrEqual(t, len(out.InternetGateways), 1)
}

func TestNATSIGWService_DeleteInternetGateway(t *testing.T) {
	client, _ := setupNATSIGWServiceTest(t)

	createOut, err := client.CreateInternetGateway(context.Background(), &ec2.CreateInternetGatewayInput{}, testAccountID)
	require.NoError(t, err)

	_, err = client.DeleteInternetGateway(context.Background(), &ec2.DeleteInternetGatewayInput{
		InternetGatewayId: createOut.InternetGateway.InternetGatewayId,
	}, testAccountID)
	require.NoError(t, err)
}

func TestNATSIGWService_AttachAndDetach(t *testing.T) {
	client, _ := setupNATSIGWServiceTest(t)

	createOut, err := client.CreateInternetGateway(context.Background(), &ec2.CreateInternetGatewayInput{}, testAccountID)
	require.NoError(t, err)
	igwID := createOut.InternetGateway.InternetGatewayId

	_, err = client.AttachInternetGateway(context.Background(), &ec2.AttachInternetGatewayInput{
		InternetGatewayId: igwID,
		VpcId:             aws.String("vpc-test123"),
	}, testAccountID)
	require.NoError(t, err)

	_, err = client.DetachInternetGateway(context.Background(), &ec2.DetachInternetGatewayInput{
		InternetGatewayId: igwID,
		VpcId:             aws.String("vpc-test123"),
	}, testAccountID)
	require.NoError(t, err)
}
