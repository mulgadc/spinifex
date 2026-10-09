package networkv1_test

import (
	"encoding/json"
	"testing"

	networkv1 "github.com/mulgadc/spinifex/contracts/network/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPortSecurityGroupsUpdateSubject(t *testing.T) {
	assert.Equal(t, "vpc.update-port-sgs", networkv1.PortSecurityGroupsUpdateSubject)
}

func TestPortSecurityGroupsUpdateEvent_JSON(t *testing.T) {
	golden := `{"network_interface_id":"eni-1","private_ip_address":"10.0.1.5","security_group_ids":["sg-1","sg-2"]}`
	evt := networkv1.PortSecurityGroupsUpdateEvent{
		NetworkInterfaceId: "eni-1", PrivateIpAddress: "10.0.1.5",
		SecurityGroupIds: []string{"sg-1", "sg-2"},
	}

	marshaled, err := json.Marshal(evt)
	require.NoError(t, err)
	assert.JSONEq(t, golden, string(marshaled))

	var decoded networkv1.PortSecurityGroupsUpdateEvent
	require.NoError(t, json.Unmarshal([]byte(golden), &decoded))
	assert.Equal(t, evt, decoded)
}
