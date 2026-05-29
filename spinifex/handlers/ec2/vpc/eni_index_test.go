package handlers_ec2_vpc

import (
	"encoding/json"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	handlers_imds "github.com/mulgadc/spinifex/spinifex/handlers/imds"
	"github.com/nats-io/nats.go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// openTestENIByIPBucket opens the eni-by-vpc-ip reverse-index bucket on the
// test JetStream, exercising the same init path the daemon uses.
func openTestENIByIPBucket(t *testing.T, nc *nats.Conn) nats.KeyValue {
	t.Helper()
	js, err := nc.JetStream()
	require.NoError(t, err)
	kv, err := handlers_imds.InitENIByIPBucket(js, 1)
	require.NoError(t, err)
	return kv
}

// readIndexENIID reads the raw vpcID/ip entry and returns its eni_id, or ""
// when the key is absent.
func readIndexENIID(t *testing.T, kv nats.KeyValue, vpcID, ip string) string {
	t.Helper()
	entry, err := kv.Get(vpcID + "/" + ip)
	if err != nil {
		require.ErrorIs(t, err, nats.ErrKeyNotFound)
		return ""
	}
	var v struct {
		ENIId string `json:"eni_id"`
	}
	require.NoError(t, json.Unmarshal(entry.Value(), &v))
	return v.ENIId
}

func TestENIByIPIndex_PutGetDelete(t *testing.T) {
	_, nc := setupTestVPCServiceWithNC(t)
	kv := openTestENIByIPBucket(t, nc)
	idx := NewENIByIPIndex(kv)

	require.NoError(t, idx.Put("vpc-abc12345", "10.0.1.5", "eni-deadbeef"))
	assert.Equal(t, "eni-deadbeef", readIndexENIID(t, kv, "vpc-abc12345", "10.0.1.5"))

	// Overwrite is allowed (rebind is a Put).
	require.NoError(t, idx.Put("vpc-abc12345", "10.0.1.5", "eni-cafef00d"))
	assert.Equal(t, "eni-cafef00d", readIndexENIID(t, kv, "vpc-abc12345", "10.0.1.5"))

	require.NoError(t, idx.Delete("vpc-abc12345", "10.0.1.5"))
	assert.Equal(t, "", readIndexENIID(t, kv, "vpc-abc12345", "10.0.1.5"))
}

func TestENIByIPIndex_DeleteAbsentIsIdempotent(t *testing.T) {
	_, nc := setupTestVPCServiceWithNC(t)
	idx := NewENIByIPIndex(openTestENIByIPBucket(t, nc))
	require.NoError(t, idx.Delete("vpc-nope", "10.0.0.1"))
}

func TestENIByIPIndex_KeysAreVPCScoped(t *testing.T) {
	// Same IP in two VPCs must not collide — the key includes the VPC ID.
	_, nc := setupTestVPCServiceWithNC(t)
	kv := openTestENIByIPBucket(t, nc)
	idx := NewENIByIPIndex(kv)

	require.NoError(t, idx.Put("vpc-aaaaaaaa", "10.0.1.5", "eni-aaa"))
	require.NoError(t, idx.Put("vpc-bbbbbbbb", "10.0.1.5", "eni-bbb"))

	assert.Equal(t, "eni-aaa", readIndexENIID(t, kv, "vpc-aaaaaaaa", "10.0.1.5"))
	assert.Equal(t, "eni-bbb", readIndexENIID(t, kv, "vpc-bbbbbbbb", "10.0.1.5"))
}

func TestCreateNetworkInterface_WritesENIIndex(t *testing.T) {
	svc, nc := setupTestVPCServiceWithNC(t)
	kv := openTestENIByIPBucket(t, nc)
	svc.SetENIByIPIndex(NewENIByIPIndex(kv))

	vpcID := createTestVPC(t, svc, "10.0.0.0/16")
	subnetID := createTestSubnet(t, svc, vpcID, "10.0.1.0/24")

	out, err := svc.CreateNetworkInterface(&ec2.CreateNetworkInterfaceInput{
		SubnetId: aws.String(subnetID),
	}, testAccountID)
	require.NoError(t, err)

	eniID := *out.NetworkInterface.NetworkInterfaceId
	ip := *out.NetworkInterface.PrivateIpAddress
	assert.Equal(t, eniID, readIndexENIID(t, kv, vpcID, ip))
}

func TestDeleteNetworkInterface_RemovesENIIndex(t *testing.T) {
	svc, nc := setupTestVPCServiceWithNC(t)
	kv := openTestENIByIPBucket(t, nc)
	svc.SetENIByIPIndex(NewENIByIPIndex(kv))

	vpcID := createTestVPC(t, svc, "10.0.0.0/16")
	subnetID := createTestSubnet(t, svc, vpcID, "10.0.1.0/24")

	out, err := svc.CreateNetworkInterface(&ec2.CreateNetworkInterfaceInput{
		SubnetId: aws.String(subnetID),
	}, testAccountID)
	require.NoError(t, err)
	eniID := *out.NetworkInterface.NetworkInterfaceId
	ip := *out.NetworkInterface.PrivateIpAddress
	require.Equal(t, eniID, readIndexENIID(t, kv, vpcID, ip))

	_, err = svc.DeleteNetworkInterface(&ec2.DeleteNetworkInterfaceInput{
		NetworkInterfaceId: aws.String(eniID),
	}, testAccountID)
	require.NoError(t, err)

	assert.Equal(t, "", readIndexENIID(t, kv, vpcID, ip))
}

// Regression: ENI lifecycle must not panic or error when no index is wired
// (IMDS-less deployments, focused tests).
func TestCreateDeleteNetworkInterface_NoIndexWired(t *testing.T) {
	svc := setupTestVPCService(t)
	vpcID := createTestVPC(t, svc, "10.0.0.0/16")
	subnetID := createTestSubnet(t, svc, vpcID, "10.0.1.0/24")

	out, err := svc.CreateNetworkInterface(&ec2.CreateNetworkInterfaceInput{
		SubnetId: aws.String(subnetID),
	}, testAccountID)
	require.NoError(t, err)

	_, err = svc.DeleteNetworkInterface(&ec2.DeleteNetworkInterfaceInput{
		NetworkInterfaceId: out.NetworkInterface.NetworkInterfaceId,
	}, testAccountID)
	require.NoError(t, err)
}
