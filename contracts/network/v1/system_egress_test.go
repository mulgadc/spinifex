package networkv1_test

import (
	"encoding/json"
	"testing"

	networkv1 "github.com/mulgadc/spinifex/contracts/network/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSystemEgressSubjects(t *testing.T) {
	assert.Equal(t, "vpc.add-system-egress", networkv1.SystemEgressAddSubject)
	assert.Equal(t, "vpc.delete-system-egress", networkv1.SystemEgressDeleteSubject)
}

func TestSystemEgressEvent_JSON(t *testing.T) {
	golden := `{"vpc_id":"vpc-1","subnet_id":"subnet-1","instance_ip":"10.0.1.5","external_ip":"203.0.113.5"}`
	evt := networkv1.SystemEgressEvent{
		VpcId: "vpc-1", SubnetId: "subnet-1", InstanceIp: "10.0.1.5", ExternalIp: "203.0.113.5",
	}

	marshaled, err := json.Marshal(evt)
	require.NoError(t, err)
	assert.JSONEq(t, golden, string(marshaled))

	var decoded networkv1.SystemEgressEvent
	require.NoError(t, json.Unmarshal([]byte(golden), &decoded))
	assert.Equal(t, evt, decoded)
}
