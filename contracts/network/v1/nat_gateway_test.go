package networkv1_test

import (
	"encoding/json"
	"testing"

	networkv1 "github.com/mulgadc/spinifex/contracts/network/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNATGatewaySubjects(t *testing.T) {
	assert.Equal(t, "vpc.add-nat-gateway", networkv1.NATGatewayAddSubject)
	assert.Equal(t, "vpc.delete-nat-gateway", networkv1.NATGatewayDeleteSubject)
}

func TestNATGatewayEvent_JSON(t *testing.T) {
	golden := `{"vpc_id":"vpc-1","nat_gateway_id":"nat-1","public_ip":"203.0.113.5","subnet_cidr":"10.0.1.0/24","subnet_id":"subnet-1","destination_cidr":"0.0.0.0/0"}`
	evt := networkv1.NATGatewayEvent{
		VpcId: "vpc-1", NatGatewayId: "nat-1", PublicIp: "203.0.113.5",
		SubnetCidr: "10.0.1.0/24", SubnetId: "subnet-1", DestinationCidr: "0.0.0.0/0",
	}

	marshaled, err := json.Marshal(evt)
	require.NoError(t, err)
	assert.JSONEq(t, golden, string(marshaled))

	var decoded networkv1.NATGatewayEvent
	require.NoError(t, json.Unmarshal([]byte(golden), &decoded))
	assert.Equal(t, evt, decoded)
}

// TestNATGatewayEvent_NarrowPublisherShapeStillDecodes pins that the
// narrower 4-field shape one of today's two publishers sends (domains/ec2/
// natgw) still decodes correctly: the missing fields zero-value, which is
// what the subscriber's own equivalent type already does today.
func TestNATGatewayEvent_NarrowPublisherShapeStillDecodes(t *testing.T) {
	narrow := `{"vpc_id":"vpc-1","nat_gateway_id":"nat-1","public_ip":"203.0.113.5","subnet_cidr":"10.0.1.0/24"}`
	var decoded networkv1.NATGatewayEvent
	require.NoError(t, json.Unmarshal([]byte(narrow), &decoded))
	assert.Equal(t, networkv1.NATGatewayEvent{
		VpcId: "vpc-1", NatGatewayId: "nat-1", PublicIp: "203.0.113.5", SubnetCidr: "10.0.1.0/24",
	}, decoded)
}
