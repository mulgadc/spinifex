package networkv1_test

import (
	"encoding/json"
	"testing"

	networkv1 "github.com/mulgadc/spinifex/contracts/network/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPortSubjects(t *testing.T) {
	assert.Equal(t, "vpc.create-port", networkv1.PortCreateSubject)
	assert.Equal(t, "vpc.delete-port", networkv1.PortDeleteSubject)
}

func TestPortEvent_JSON(t *testing.T) {
	golden := `{"network_interface_id":"eni-1","subnet_id":"subnet-1","vpc_id":"vpc-1","private_ip_address":"10.0.1.5","mac_address":"02:00:00:00:00:01","security_group_ids":["sg-1"],"suppress_dhcp":true}`
	evt := networkv1.PortEvent{
		NetworkInterfaceId: "eni-1", SubnetId: "subnet-1", VpcId: "vpc-1",
		PrivateIpAddress: "10.0.1.5", MacAddress: "02:00:00:00:00:01",
		SecurityGroupIds: []string{"sg-1"}, SuppressDHCP: true,
	}

	marshaled, err := json.Marshal(evt)
	require.NoError(t, err)
	assert.JSONEq(t, golden, string(marshaled))

	var decoded networkv1.PortEvent
	require.NoError(t, json.Unmarshal([]byte(golden), &decoded))
	assert.Equal(t, evt, decoded)
}

func TestPortEvent_OmitsEmptyFields(t *testing.T) {
	golden := `{"network_interface_id":"eni-1","subnet_id":"subnet-1","vpc_id":"vpc-1","private_ip_address":"10.0.1.5","mac_address":"02:00:00:00:00:01"}`
	evt := networkv1.PortEvent{
		NetworkInterfaceId: "eni-1", SubnetId: "subnet-1", VpcId: "vpc-1",
		PrivateIpAddress: "10.0.1.5", MacAddress: "02:00:00:00:00:01",
	}

	marshaled, err := json.Marshal(evt)
	require.NoError(t, err)
	assert.JSONEq(t, golden, string(marshaled))
}
