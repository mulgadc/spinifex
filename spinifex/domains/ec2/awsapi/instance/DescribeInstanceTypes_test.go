package instance

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
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

func TestDescribeInstanceTypes_CapacityFilterShowsDuplicates(t *testing.T) {
	t.Parallel()
	_, nc := startTestNATSServer(t)

	// Node 1
	nc.Subscribe("ec2.DescribeInstanceTypes", func(msg *nats.Msg) {
		data, _ := json.Marshal(&ec2.DescribeInstanceTypesOutput{
			InstanceTypes: []*ec2.InstanceTypeInfo{
				{InstanceType: aws.String("t3.micro")},
			},
		})
		msg.Respond(data)
	})

	// Node 2 reports same type
	nc2, err := nats.Connect(nc.ConnectedUrl())
	require.NoError(t, err)
	defer nc2.Close()

	nc2.Subscribe("ec2.DescribeInstanceTypes", func(msg *nats.Msg) {
		data, _ := json.Marshal(&ec2.DescribeInstanceTypesOutput{
			InstanceTypes: []*ec2.InstanceTypeInfo{
				{InstanceType: aws.String("t3.micro")},
			},
		})
		msg.Respond(data)
	})

	nc.Flush()
	nc2.Flush()

	// With capacity filter = true, duplicates should be preserved
	input := &ec2.DescribeInstanceTypesInput{
		Filters: []*ec2.Filter{
			{
				Name:   aws.String("capacity"),
				Values: []*string{aws.String("true")},
			},
		},
	}
	output, err := DescribeInstanceTypes(context.Background(), input, nc, 2, nil, "")

	require.NoError(t, err)
	require.NotNil(t, output)
	// Both slots should appear since capacity filter is enabled
	assert.Len(t, output.InstanceTypes, 2)
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
