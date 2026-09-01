package handlers_ec2_vpc

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/nats-io/nats.go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// stubAddNAT answers vpc.add-nat, which RebindENIPublicIP drives request-reply.
// Captured external IPs let a test assert the NAT followed the record.
func stubAddNAT(t *testing.T, nc *nats.Conn, fail string) *[]string {
	t.Helper()
	seen := &[]string{}
	sub, err := nc.Subscribe("vpc.add-nat", func(msg *nats.Msg) {
		var evt struct {
			ExternalIP string `json:"external_ip"`
		}
		_ = json.Unmarshal(msg.Data, &evt)
		*seen = append(*seen, evt.ExternalIP)
		reply := map[string]any{"success": fail == "", "error": fail}
		data, _ := json.Marshal(reply)
		require.NoError(t, msg.Respond(data))
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = sub.Unsubscribe() })
	return seen
}

func TestRebindENIPublicIP_MovesRecordAndNAT(t *testing.T) {
	t.Parallel()
	svc, nc := setupTestVPCServiceWithNC(t)
	seen := stubAddNAT(t, nc, "")
	vpcID := createTestVPC(t, svc, "10.0.0.0/16")
	subnetID := createTestSubnet(t, svc, vpcID, "10.0.1.0/24")
	eniID := createTestENI(t, svc, subnetID)
	require.NoError(t, svc.UpdateENIPublicIP(testAccountID, eniID, "192.168.1.135", "wan"))

	require.NoError(t, svc.RebindENIPublicIP(t.Context(), eniID, "192.168.1.135", "192.168.1.146"))

	out, err := svc.DescribeNetworkInterfaces(context.Background(), &ec2.DescribeNetworkInterfacesInput{
		NetworkInterfaceIds: []*string{aws.String(eniID)},
	}, testAccountID)
	require.NoError(t, err)
	require.Len(t, out.NetworkInterfaces, 1)
	require.NotNil(t, out.NetworkInterfaces[0].Association)
	assert.Equal(t, "192.168.1.146", *out.NetworkInterfaces[0].Association.PublicIp,
		"DescribeNetworkInterfaces must not keep reporting an address vpcd released")
	assert.Contains(t, *seen, "192.168.1.146", "the dnat_and_snat must follow the record")
}

// A NAT commit failure leaves the instance with no working ingress, so the
// record must not be moved to advertise an address that does not route.
func TestRebindENIPublicIP_NATFailureLeavesRecord(t *testing.T) {
	t.Parallel()
	svc, nc := setupTestVPCServiceWithNC(t)
	stubAddNAT(t, nc, "ovn unreachable")
	vpcID := createTestVPC(t, svc, "10.0.0.0/16")
	subnetID := createTestSubnet(t, svc, vpcID, "10.0.1.0/24")
	eniID := createTestENI(t, svc, subnetID)
	require.NoError(t, svc.UpdateENIPublicIP(testAccountID, eniID, "192.168.1.135", "wan"))

	err := svc.RebindENIPublicIP(t.Context(), eniID, "192.168.1.135", "192.168.1.146")
	require.Error(t, err)

	record, _, _, findErr := svc.findENIAnyAccount(t.Context(), eniID)
	require.NoError(t, findErr)
	assert.Equal(t, "192.168.1.135", record.PublicIpAddress)
}

func TestRebindENIPublicIP_IsIdempotent(t *testing.T) {
	t.Parallel()
	svc, nc := setupTestVPCServiceWithNC(t)
	stubAddNAT(t, nc, "")
	vpcID := createTestVPC(t, svc, "10.0.0.0/16")
	subnetID := createTestSubnet(t, svc, vpcID, "10.0.1.0/24")
	eniID := createTestENI(t, svc, subnetID)
	require.NoError(t, svc.UpdateENIPublicIP(testAccountID, eniID, "192.168.1.135", "wan"))

	require.NoError(t, svc.RebindENIPublicIP(t.Context(), eniID, "192.168.1.135", "192.168.1.146"))
	require.NoError(t, svc.RebindENIPublicIP(t.Context(), eniID, "192.168.1.135", "192.168.1.146"))
}

func TestRebindENIPublicIP_RejectsMismatchAndUnknown(t *testing.T) {
	t.Parallel()
	svc, nc := setupTestVPCServiceWithNC(t)
	stubAddNAT(t, nc, "")
	vpcID := createTestVPC(t, svc, "10.0.0.0/16")
	subnetID := createTestSubnet(t, svc, vpcID, "10.0.1.0/24")
	eniID := createTestENI(t, svc, subnetID)
	require.NoError(t, svc.UpdateENIPublicIP(testAccountID, eniID, "192.168.1.135", "wan"))

	err := svc.RebindENIPublicIP(t.Context(), eniID, "203.0.113.7", "192.168.1.146")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not the superseded 203.0.113.7")

	err = svc.RebindENIPublicIP(t.Context(), "eni-missing", "192.168.1.135", "192.168.1.146")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no ENI record for eni-missing")
}
