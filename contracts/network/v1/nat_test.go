package networkv1_test

import (
	"encoding/json"
	"testing"

	networkv1 "github.com/mulgadc/spinifex/contracts/network/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNATSubjects(t *testing.T) {
	assert.Equal(t, "vpc.add-nat", networkv1.NATAddSubject)
	assert.Equal(t, "vpc.delete-nat", networkv1.NATDeleteSubject)
}

func TestNATEvent_JSON(t *testing.T) {
	golden := `{"vpc_id":"vpc-1","external_ip":"203.0.113.5","logical_ip":"10.0.1.5","port_name":"port-eni-1","mac":"02:00:00:00:00:01"}`
	evt := networkv1.NATEvent{
		VpcId: "vpc-1", ExternalIP: "203.0.113.5", LogicalIP: "10.0.1.5",
		PortName: "port-eni-1", MAC: "02:00:00:00:00:01",
	}

	marshaled, err := json.Marshal(evt)
	require.NoError(t, err)
	assert.JSONEq(t, golden, string(marshaled))

	var decoded networkv1.NATEvent
	require.NoError(t, json.Unmarshal([]byte(golden), &decoded))
	assert.Equal(t, evt, decoded)
}
