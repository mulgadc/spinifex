package networkv1_test

import (
	"encoding/json"
	"testing"

	networkv1 "github.com/mulgadc/spinifex/contracts/network/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSubnetEgressGateSubjects(t *testing.T) {
	assert.Equal(t, "vpc.gate-subnet-egress", networkv1.SubnetEgressGateSubject)
	assert.Equal(t, "vpc.ungate-subnet-egress", networkv1.SubnetEgressUngateSubject)
}

func TestSubnetEgressGateEvent_JSON(t *testing.T) {
	golden := `{"vpc_id":"vpc-1","subnet_id":"subnet-1","destination_cidr":"0.0.0.0/0"}`
	evt := networkv1.SubnetEgressGateEvent{VpcId: "vpc-1", SubnetId: "subnet-1", DestinationCidr: "0.0.0.0/0"}

	marshaled, err := json.Marshal(evt)
	require.NoError(t, err)
	assert.JSONEq(t, golden, string(marshaled))

	var decoded networkv1.SubnetEgressGateEvent
	require.NoError(t, json.Unmarshal([]byte(golden), &decoded))
	assert.Equal(t, evt, decoded)
}

func TestSubnetEgressUngateEvent_JSON(t *testing.T) {
	golden := `{"vpc_id":"vpc-1","subnet_id":"subnet-1","destination_cidr":"0.0.0.0/0"}`
	evt := networkv1.SubnetEgressUngateEvent{VpcId: "vpc-1", SubnetId: "subnet-1", DestinationCidr: "0.0.0.0/0"}

	marshaled, err := json.Marshal(evt)
	require.NoError(t, err)
	assert.JSONEq(t, golden, string(marshaled))

	var decoded networkv1.SubnetEgressUngateEvent
	require.NoError(t, json.Unmarshal([]byte(golden), &decoded))
	assert.Equal(t, evt, decoded)
}
