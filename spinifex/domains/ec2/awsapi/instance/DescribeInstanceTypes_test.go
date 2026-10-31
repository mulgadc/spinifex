package instance

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	instancesvc "github.com/mulgadc/spinifex/spinifex/domains/ec2/instance"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/nats-io/nats.go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDescribeInstanceTypes_SingleNode(t *testing.T) {
	t.Parallel()
	_, nc := startTestNATSServer(t)

	nc.Subscribe("ec2.DescribeInstanceTypes", func(msg *nats.Msg) {
		data, _ := json.Marshal(&ec2.DescribeInstanceTypesOutput{
			InstanceTypes: []*ec2.InstanceTypeInfo{
				{InstanceType: aws.String("t3.micro")},
				{InstanceType: aws.String("t3.small")},
			},
		})
		msg.Respond(data)
	})

	input := &ec2.DescribeInstanceTypesInput{}
	output, err := DescribeInstanceTypes(context.Background(), input, nc, 1, nil, "")

	require.NoError(t, err)
	require.NotNil(t, output)
	assert.Len(t, output.InstanceTypes, 2)
	assert.Equal(t, "t3.micro", *output.InstanceTypes[0].InstanceType)
	assert.Equal(t, "t3.small", *output.InstanceTypes[1].InstanceType)
}

func TestDescribeInstanceTypes_DeduplicatesAcrossNodes(t *testing.T) {
	t.Parallel()
	_, nc := startTestNATSServer(t)

	// Node 1
	nc.Subscribe("ec2.DescribeInstanceTypes", func(msg *nats.Msg) {
		data, _ := json.Marshal(&ec2.DescribeInstanceTypesOutput{
			InstanceTypes: []*ec2.InstanceTypeInfo{
				{InstanceType: aws.String("t3.micro")},
				{InstanceType: aws.String("t3.small")},
			},
		})
		msg.Respond(data)
	})

	// Node 2 reports same types
	nc2, err := nats.Connect(nc.ConnectedUrl())
	require.NoError(t, err)
	defer nc2.Close()

	nc2.Subscribe("ec2.DescribeInstanceTypes", func(msg *nats.Msg) {
		data, _ := json.Marshal(&ec2.DescribeInstanceTypesOutput{
			InstanceTypes: []*ec2.InstanceTypeInfo{
				{InstanceType: aws.String("t3.micro")},
				{InstanceType: aws.String("m5.large")},
			},
		})
		msg.Respond(data)
	})

	nc.Flush()
	nc2.Flush()

	input := &ec2.DescribeInstanceTypesInput{}
	output, err := DescribeInstanceTypes(context.Background(), input, nc, 2, nil, "")

	require.NoError(t, err)
	require.NotNil(t, output)
	// t3.micro should appear once, t3.small once, m5.large once = 3 unique
	assert.Len(t, output.InstanceTypes, 3)

	seen := make(map[string]int)
	for _, it := range output.InstanceTypes {
		seen[*it.InstanceType]++
	}
	assert.Equal(t, 1, seen["t3.micro"])
	assert.Equal(t, 1, seen["t3.small"])
	assert.Equal(t, 1, seen["m5.large"])
}

// Every node describes the types it hosts, so a four-node cluster sends four
// copies. The listing reports each type once.
func TestDescribeInstanceTypes_CapacityReportsEachTypeOncePerCluster(t *testing.T) {
	t.Parallel()
	_, nc := startTestNATSServer(t)

	// Node 1
	nc.Subscribe("ec2.DescribeInstanceTypes", func(msg *nats.Msg) {
		msg.Respond(capacityPayload(t, map[string]int64{"t3.micro": 1}))
	})

	// Node 2 reports the same type
	nc2, err := nats.Connect(nc.ConnectedUrl())
	require.NoError(t, err)
	defer nc2.Close()

	nc2.Subscribe("ec2.DescribeInstanceTypes", func(msg *nats.Msg) {
		msg.Respond(capacityPayload(t, map[string]int64{"t3.micro": 1}))
	})

	nc.Flush()
	nc2.Flush()

	output, err := DescribeInstanceTypes(context.Background(), capacityInput(), nc, 2, nil, "")

	require.NoError(t, err)
	require.NotNil(t, output)
	assert.Len(t, output.InstanceTypes, 1)
	assert.Equal(t, "t3.micro", aws.StringValue(output.InstanceTypes[0].InstanceType))
}

// The reply is bounded by the catalogue, so its size is the same whether the
// cluster is idle or full. This is the property the per-slot encoding lost: at
// 44,346 free slots the same shape exceeded the NATS maximum payload and the
// console was told the cluster had no compute at all.
func TestDescribeInstanceTypes_CapacityIsBoundedByTypeCountNotFreeSlots(t *testing.T) {
	t.Parallel()

	for name, slots := range map[string]int64{"idle": 44346, "nearly full": 1} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			_, nc := startTestNATSServer(t)

			payload := capacityPayload(t, map[string]int64{"t3.micro": slots})
			require.Less(t, len(payload), 4096, "the node reply must not grow with free capacity")

			subscribeAsNode(t, nc, "ec2.DescribeInstanceTypes", "n1", payload)

			output, err := DescribeInstanceTypes(context.Background(), capacityInput(), nc, 1, nil, "")
			require.NoError(t, err)
			assert.Len(t, output.InstanceTypes, 1)
		})
	}
}

// A type the node could not admit is absent from the listing, so the console
// does not offer a type the cluster would refuse.
func TestDescribeInstanceTypes_CapacityOmitsTypesWithNoFreeSlot(t *testing.T) {
	t.Parallel()
	_, nc := startTestNATSServer(t)

	subscribeAsNode(t, nc, "ec2.DescribeInstanceTypes", "n1",
		capacityPayload(t, map[string]int64{"t3.micro": 4, "x2gd.metal": 0}))

	output, err := DescribeInstanceTypes(context.Background(), capacityInput(), nc, 1, nil, "")

	require.NoError(t, err)
	require.Len(t, output.InstanceTypes, 1)
	assert.Equal(t, "t3.micro", aws.StringValue(output.InstanceTypes[0].InstanceType))
}

// Nothing is known about capacity when no node answered, so the request fails
// rather than reporting an empty list the console reads as no capacity.
func TestDescribeInstanceTypes_CapacityWithNoResponsesIsRetryable(t *testing.T) {
	t.Parallel()
	_, nc := startTestNATSServer(t)

	_, err := DescribeInstanceTypes(context.Background(), capacityInput(), nc, 0, nil, "")

	code, _, ok := awserrors.ResolveErrorDetail(err)
	require.True(t, ok)
	assert.Equal(t, awserrors.ErrorServiceUnavailable, code)
}

func capacityInput() *ec2.DescribeInstanceTypesInput {
	return &ec2.DescribeInstanceTypesInput{
		Filters: []*ec2.Filter{{Name: aws.String("capacity"), Values: []*string{aws.String("true")}}},
	}
}

func capacityPayload(t *testing.T, availability map[string]int64) []byte {
	t.Helper()
	out := &instancesvc.DescribeInstanceTypesNodeOutput{Availability: availability}
	for name := range availability {
		out.InstanceTypes = append(out.InstanceTypes, &ec2.InstanceTypeInfo{InstanceType: aws.String(name)})
	}
	data, err := json.Marshal(out)
	require.NoError(t, err)
	return data
}

func TestDescribeInstanceTypes_CapacityFilterFalseDeduplicates(t *testing.T) {
	t.Parallel()
	_, nc := startTestNATSServer(t)

	nc.Subscribe("ec2.DescribeInstanceTypes", func(msg *nats.Msg) {
		data, _ := json.Marshal(&ec2.DescribeInstanceTypesOutput{
			InstanceTypes: []*ec2.InstanceTypeInfo{
				{InstanceType: aws.String("t3.micro")},
				{InstanceType: aws.String("t3.micro")},
			},
		})
		msg.Respond(data)
	})

	// capacity=false should still deduplicate
	input := &ec2.DescribeInstanceTypesInput{
		Filters: []*ec2.Filter{
			{
				Name:   aws.String("capacity"),
				Values: []*string{aws.String("false")},
			},
		},
	}
	output, err := DescribeInstanceTypes(context.Background(), input, nc, 1, nil, "")

	require.NoError(t, err)
	require.NotNil(t, output)
	assert.Len(t, output.InstanceTypes, 1)
}

func TestDescribeInstanceTypes_NoSubscribers(t *testing.T) {
	t.Parallel()
	_, nc := startTestNATSServer(t)

	input := &ec2.DescribeInstanceTypesInput{}
	output, err := DescribeInstanceTypes(context.Background(), input, nc, 0, nil, "")

	require.NoError(t, err)
	require.NotNil(t, output)
	assert.Empty(t, output.InstanceTypes)
}

func TestDescribeInstanceTypes_NodeReturnsError(t *testing.T) {
	t.Parallel()
	_, nc := startTestNATSServer(t)

	nc.Subscribe("ec2.DescribeInstanceTypes", func(msg *nats.Msg) {
		errorPayload := awserrors.GenerateErrorPayload("InternalError")
		msg.Respond(errorPayload)
	})

	input := &ec2.DescribeInstanceTypesInput{}
	output, err := DescribeInstanceTypes(context.Background(), input, nc, 1, nil, "")

	require.NoError(t, err)
	require.NotNil(t, output)
	assert.Empty(t, output.InstanceTypes)
}

func TestDescribeInstanceTypes_MalformedJSON(t *testing.T) {
	t.Parallel()
	_, nc := startTestNATSServer(t)

	nc.Subscribe("ec2.DescribeInstanceTypes", func(msg *nats.Msg) {
		msg.Respond([]byte(`not valid json`))
	})

	input := &ec2.DescribeInstanceTypesInput{}
	output, err := DescribeInstanceTypes(context.Background(), input, nc, 1, nil, "")

	require.NoError(t, err)
	require.NotNil(t, output)
	assert.Empty(t, output.InstanceTypes)
}

func TestDescribeInstanceTypes_NilInstanceTypeSkipped(t *testing.T) {
	t.Parallel()
	_, nc := startTestNATSServer(t)

	nc.Subscribe("ec2.DescribeInstanceTypes", func(msg *nats.Msg) {
		data, _ := json.Marshal(&ec2.DescribeInstanceTypesOutput{
			InstanceTypes: []*ec2.InstanceTypeInfo{
				{InstanceType: aws.String("t3.micro")},
				{InstanceType: nil}, // nil InstanceType should be skipped in dedup
				nil,                 // nil entry should be skipped
			},
		})
		msg.Respond(data)
	})

	input := &ec2.DescribeInstanceTypesInput{}
	output, err := DescribeInstanceTypes(context.Background(), input, nc, 1, nil, "")

	require.NoError(t, err)
	require.NotNil(t, output)
	assert.Len(t, output.InstanceTypes, 1)
	assert.Equal(t, "t3.micro", *output.InstanceTypes[0].InstanceType)
}

func TestDescribeInstanceTypes_ClosedConnection(t *testing.T) {
	t.Parallel()
	_, nc := startTestNATSServer(t)

	closedNC, err := nats.Connect(nc.ConnectedUrl())
	require.NoError(t, err)
	closedNC.Close()

	input := &ec2.DescribeInstanceTypesInput{}
	_, err = DescribeInstanceTypes(context.Background(), input, closedNC, 1, nil, "")

	require.Error(t, err)
}

func instanceTypesPayload(t *testing.T, names ...string) []byte {
	t.Helper()
	out := &ec2.DescribeInstanceTypesOutput{}
	for _, n := range names {
		out.InstanceTypes = append(out.InstanceTypes, &ec2.InstanceTypeInfo{InstanceType: aws.String(n)})
	}
	data, err := json.Marshal(out)
	require.NoError(t, err)
	return data
}

func instanceTypeNames(out *ec2.DescribeInstanceTypesOutput) []string {
	var names []string
	for _, it := range out.InstanceTypes {
		names = append(names, *it.InstanceType)
	}
	return names
}

// AWS pages on MaxResults and NextToken; the aggregate is sorted so a page
// boundary holds across calls whatever order the nodes reply in.
func TestDescribeInstanceTypes_Paging(t *testing.T) {
	t.Parallel()
	_, nc := startTestNATSServer(t)
	subscribeAsNode(t, nc, "ec2.DescribeInstanceTypes", "n1",
		instanceTypesPayload(t, "t3.small", "m5.large", "t3.micro", "c5.large", "t3.nano", "m5.xlarge", "c5.xlarge"))

	page1, err := DescribeInstanceTypes(context.Background(), &ec2.DescribeInstanceTypesInput{MaxResults: aws.Int64(5)}, nc, 1, nil, "")
	require.NoError(t, err)
	assert.Equal(t, []string{"c5.large", "c5.xlarge", "m5.large", "m5.xlarge", "t3.micro"}, instanceTypeNames(page1))
	require.NotNil(t, page1.NextToken)

	page2, err := DescribeInstanceTypes(context.Background(), &ec2.DescribeInstanceTypesInput{MaxResults: aws.Int64(5), NextToken: page1.NextToken}, nc, 1, nil, "")
	require.NoError(t, err)
	assert.Equal(t, []string{"t3.nano", "t3.small"}, instanceTypeNames(page2))
	assert.Nil(t, page2.NextToken)

	all, err := DescribeInstanceTypes(context.Background(), &ec2.DescribeInstanceTypesInput{}, nc, 1, nil, "")
	require.NoError(t, err)
	assert.Len(t, all.InstanceTypes, 7)
	assert.Nil(t, all.NextToken)
}

func TestDescribeInstanceTypes_InstanceTypeFilter(t *testing.T) {
	t.Parallel()
	_, nc := startTestNATSServer(t)
	subscribeAsNode(t, nc, "ec2.DescribeInstanceTypes", "n1",
		instanceTypesPayload(t, "t3.micro", "t3.small", "m5.large", "c5.large"))

	for _, tc := range []struct {
		values []string
		want   []string
	}{
		{[]string{"t3.micro"}, []string{"t3.micro"}},
		{[]string{"t3.*"}, []string{"t3.micro", "t3.small"}},
		{[]string{"m5.large", "c5.large"}, []string{"c5.large", "m5.large"}},
		{[]string{"zz9.bogus"}, nil},
	} {
		out, err := DescribeInstanceTypes(context.Background(), &ec2.DescribeInstanceTypesInput{
			Filters: []*ec2.Filter{{Name: aws.String("instance-type"), Values: aws.StringSlice(tc.values)}},
		}, nc, 1, nil, "")
		require.NoError(t, err)
		assert.Equal(t, tc.want, instanceTypeNames(out), tc.values)
	}
}

// A named type no node has is InvalidInstanceType, as on AWS, once every
// configured node has answered.
func TestDescribeInstanceTypes_UnknownNamedType(t *testing.T) {
	t.Parallel()
	_, nc := startTestNATSServer(t)
	subscribeAsNode(t, nc, "ec2.DescribeInstanceTypes", "n1", instanceTypesPayload(t, "t3.micro"))
	subscribeAsNode(t, nc, "ec2.DescribeInstanceTypes", "n2", instanceTypesPayload(t, "m5.large"))
	nodes := []string{"n1", "n2"}

	out, err := DescribeInstanceTypes(context.Background(), &ec2.DescribeInstanceTypesInput{
		InstanceTypes: aws.StringSlice([]string{"m5.large", "t3.micro"}),
	}, nc, 0, nodes, "")
	require.NoError(t, err)
	assert.Equal(t, []string{"m5.large", "t3.micro"}, instanceTypeNames(out))

	_, err = DescribeInstanceTypes(context.Background(), &ec2.DescribeInstanceTypesInput{
		InstanceTypes: aws.StringSlice([]string{"t3.micro", "zz9.bogus"}),
	}, nc, 0, nodes, "")
	code, msg, ok := awserrors.ResolveErrorDetail(err)
	require.True(t, ok)
	assert.Equal(t, awserrors.ErrorInvalidInstanceType, code)
	assert.Equal(t, "The following supplied instance types do not exist: [zz9.bogus]", msg)
}

// Without a reply from every configured node a missing type may be on the
// node that did not answer, so absence is not asserted.
func TestDescribeInstanceTypes_UnknownNamedTypeIncompleteFanout(t *testing.T) {
	t.Parallel()
	_, nc := startTestNATSServer(t)
	subscribeAsNode(t, nc, "ec2.DescribeInstanceTypes", "n1", instanceTypesPayload(t, "t3.micro"))
	subscribeAsNode(t, nc, "ec2.DescribeInstanceTypes", "n2", awserrors.GenerateErrorPayload("InternalError"))
	input := &ec2.DescribeInstanceTypesInput{InstanceTypes: aws.StringSlice([]string{"m5.large"})}

	for _, nodes := range [][]string{{"n1", "n2"}, nil} {
		_, err := DescribeInstanceTypes(context.Background(), input, nc, 2, nodes, "")
		code, _, ok := awserrors.ResolveErrorDetail(err)
		require.True(t, ok)
		assert.Equal(t, awserrors.ErrorServiceUnavailable, code, nodes)
	}
}
