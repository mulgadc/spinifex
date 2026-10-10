package networkv1_test

import (
	"encoding/json"
	"testing"

	networkv1 "github.com/mulgadc/spinifex/contracts/network/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSubnetSubjects(t *testing.T) {
	assert.Equal(t, "vpc.create-subnet", networkv1.SubnetCreateSubject)
	assert.Equal(t, "vpc.delete-subnet", networkv1.SubnetDeleteSubject)
}

func TestSubnetEvent_JSON(t *testing.T) {
	golden := `{"subnet_id":"subnet-1","vpc_id":"vpc-1","cidr_block":"10.0.1.0/24"}`
	evt := networkv1.SubnetEvent{SubnetId: "subnet-1", VpcId: "vpc-1", CidrBlock: "10.0.1.0/24"}

	marshaled, err := json.Marshal(evt)
	require.NoError(t, err)
	assert.JSONEq(t, golden, string(marshaled))

	var decoded networkv1.SubnetEvent
	require.NoError(t, json.Unmarshal([]byte(golden), &decoded))
	assert.Equal(t, evt, decoded)
}
