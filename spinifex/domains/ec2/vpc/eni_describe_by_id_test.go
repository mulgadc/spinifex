package vpc

import (
	"context"
	"errors"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	awserrors "github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// noListKV fails the test if the ENI bucket is listed.
type noListKV struct {
	jetstream.KeyValue

	t *testing.T
}

func (k noListKV) Keys(context.Context, ...jetstream.WatchOpt) ([]string, error) {
	k.t.Error("the ENI bucket was listed")
	return nil, errors.New("bucket listing refused")
}

// failGetKV fails every direct read with an error other than a missing key.
type failGetKV struct {
	jetstream.KeyValue
}

func (failGetKV) Get(context.Context, string) (jetstream.KeyValueEntry, error) {
	return nil, errors.New("replica unavailable")
}

func TestDescribeNetworkInterfaces_ByIDReadsOnlyTheNamedRecords(t *testing.T) {
	t.Parallel()
	svc := setupTestVPCService(t)
	vpcId := createTestVPC(t, svc, "10.0.0.0/16")
	subnetId := createTestSubnet(t, svc, vpcId, "10.0.1.0/24")
	eniID := createTestENI(t, svc, subnetId)
	createTestENI(t, svc, subnetId)
	svc.eniKV = noListKV{KeyValue: svc.eniKV, t: t}

	out, err := svc.DescribeNetworkInterfaces(context.Background(), &ec2.DescribeNetworkInterfacesInput{
		NetworkInterfaceIds: []*string{aws.String(eniID)},
	}, testAccountID)
	require.NoError(t, err)
	require.Len(t, out.NetworkInterfaces, 1)
	assert.Equal(t, eniID, aws.StringValue(out.NetworkInterfaces[0].NetworkInterfaceId))

	_, err = svc.DescribeNetworkInterfaces(context.Background(), &ec2.DescribeNetworkInterfacesInput{
		NetworkInterfaceIds: []*string{aws.String("eni-0123456789abcdef0")},
	}, testAccountID)
	require.Error(t, err)
	assert.Contains(t, err.Error(), awserrors.ErrorInvalidNetworkInterfaceIDNotFound)
}

func TestDescribeNetworkInterfaces_ByIDFailsClosedOnReadError(t *testing.T) {
	t.Parallel()
	svc := setupTestVPCService(t)
	vpcId := createTestVPC(t, svc, "10.0.0.0/16")
	subnetId := createTestSubnet(t, svc, vpcId, "10.0.1.0/24")
	eniID := createTestENI(t, svc, subnetId)
	svc.eniKV = failGetKV{KeyValue: svc.eniKV}

	_, err := svc.DescribeNetworkInterfaces(context.Background(), &ec2.DescribeNetworkInterfacesInput{
		NetworkInterfaceIds: []*string{aws.String(eniID)},
	}, testAccountID)
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorServerInternal, err.Error(), "a read failure must not read as a missing interface")
}
