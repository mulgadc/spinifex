package networkv1_test

import (
	"encoding/json"
	"testing"

	networkv1 "github.com/mulgadc/spinifex/contracts/network/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestVPCSubjects(t *testing.T) {
	assert.Equal(t, "vpc.create", networkv1.VPCCreateSubject)
	assert.Equal(t, "vpc.delete", networkv1.VPCDeleteSubject)
}

func TestVPCEvent_JSON(t *testing.T) {
	golden := `{"vpc_id":"vpc-1","cidr_block":"10.0.0.0/16","vni":42}`
	evt := networkv1.VPCEvent{VpcId: "vpc-1", CidrBlock: "10.0.0.0/16", VNI: 42}

	marshaled, err := json.Marshal(evt)
	require.NoError(t, err)
	assert.JSONEq(t, golden, string(marshaled))

	var decoded networkv1.VPCEvent
	require.NoError(t, json.Unmarshal([]byte(golden), &decoded))
	assert.Equal(t, evt, decoded)
}
