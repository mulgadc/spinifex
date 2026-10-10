package networkv1_test

import (
	"encoding/json"
	"testing"

	networkv1 "github.com/mulgadc/spinifex/contracts/network/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestInternetGatewaySubjects(t *testing.T) {
	assert.Equal(t, "vpc.igw-attach", networkv1.InternetGatewayAttachSubject)
	assert.Equal(t, "vpc.igw-detach", networkv1.InternetGatewayDetachSubject)
}

func TestInternetGatewayEvent_JSON(t *testing.T) {
	golden := `{"internet_gateway_id":"igw-1","vpc_id":"vpc-1"}`
	evt := networkv1.InternetGatewayEvent{InternetGatewayId: "igw-1", VpcId: "vpc-1"}

	marshaled, err := json.Marshal(evt)
	require.NoError(t, err)
	assert.JSONEq(t, golden, string(marshaled))

	var decoded networkv1.InternetGatewayEvent
	require.NoError(t, json.Unmarshal([]byte(golden), &decoded))
	assert.Equal(t, evt, decoded)
}
