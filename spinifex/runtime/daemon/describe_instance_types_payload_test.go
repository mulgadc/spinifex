package daemon

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	ec2instance "github.com/mulgadc/spinifex/spinifex/domains/ec2/instance"
	"github.com/mulgadc/spinifex/spinifex/domains/ec2/instancetypes"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// natsMaxPayload is the server's configured maximum message size. A reply
// larger than this is dropped, and the fan-out then reports no capacity.
const natsMaxPayload = 1 << 20

// A node's capacity reply is bounded by how many instance types exist, not by
// how much capacity is free, so an idle host answers as compactly as a full
// one. An earlier form sent one copy of the type per free slot, which an idle
// host grew past the NATS maximum payload.
func TestDescribeInstanceTypesCapacityReplyFitsTheNATSPayloadWhenHostIsIdle(t *testing.T) {
	catalogue := instancetypes.DetectAndGenerate(instancetypes.HostCPU{}, "x86_64", nil)
	require.NotEmpty(t, catalogue)

	// A host far larger than any in this fleet, entirely unallocated, is the
	// worst case for a reply whose size tracks free capacity.
	rm := &ResourceManager{
		hostVCPU:      4096,
		hostMemGB:     16384,
		instanceTypes: catalogue,
	}

	svc := ec2instance.NewInstanceServiceImpl(nil, catalogue, nil, nil, nil, rm, nil)

	out, err := svc.DescribeInstanceTypes(context.Background(), &ec2.DescribeInstanceTypesInput{
		Filters: []*ec2.Filter{{Name: aws.String("capacity"), Values: []*string{aws.String("true")}}},
	}, "")
	require.NoError(t, err)

	var totalSlots int64
	for _, n := range out.Availability {
		totalSlots += n
	}
	require.Greater(t, totalSlots, int64(natsMaxPayload/64),
		"the host must be idle enough that a per-slot reply would overflow")

	data, err := json.Marshal(out)
	require.NoError(t, err)
	assert.Less(t, len(data), natsMaxPayload,
		"capacity reply is %d bytes for %d free slots across %d types",
		len(data), totalSlots, len(out.InstanceTypes))
	assert.Len(t, out.Availability, len(out.InstanceTypes))
}
