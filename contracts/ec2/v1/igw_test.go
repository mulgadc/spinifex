package ec2v1

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestInternetGatewayEventJSONContract(t *testing.T) {
	data, err := json.Marshal(InternetGatewayEvent{
		InternetGatewayId: "igw-0123456789abcdef0",
		VpcId:             "vpc-0123456789abcdef0",
	})
	require.NoError(t, err)
	require.JSONEq(t, `{
		"internet_gateway_id": "igw-0123456789abcdef0",
		"vpc_id": "vpc-0123456789abcdef0"
	}`, string(data))
}

func TestInternetGatewaySubjects(t *testing.T) {
	require.Equal(t, "vpc.igw-attach", InternetGatewayAttachSubject)
	require.Equal(t, "vpc.igw-detach", InternetGatewayDetachSubject)
}
