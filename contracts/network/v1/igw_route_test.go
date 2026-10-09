package networkv1_test

import (
	"encoding/json"
	"testing"

	networkv1 "github.com/mulgadc/spinifex/contracts/network/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestIGWRouteSubjects(t *testing.T) {
	assert.Equal(t, "vpc.add-igw-route", networkv1.IGWRouteAddSubject)
	assert.Equal(t, "vpc.delete-igw-route", networkv1.IGWRouteDeleteSubject)
}

func TestIGWRouteEvent_JSON(t *testing.T) {
	golden := `{"vpc_id":"vpc-1","subnet_id":"subnet-1","destination_cidr":"0.0.0.0/0","internet_gateway_id":"igw-1"}`
	evt := networkv1.IGWRouteEvent{
		VpcId: "vpc-1", SubnetId: "subnet-1", DestinationCidr: "0.0.0.0/0", InternetGatewayId: "igw-1",
	}

	marshaled, err := json.Marshal(evt)
	require.NoError(t, err)
	assert.JSONEq(t, golden, string(marshaled))

	var decoded networkv1.IGWRouteEvent
	require.NoError(t, json.Unmarshal([]byte(golden), &decoded))
	assert.Equal(t, evt, decoded)
}
