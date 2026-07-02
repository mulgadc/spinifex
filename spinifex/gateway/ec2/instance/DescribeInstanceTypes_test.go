package gateway_ec2_instance

import (
	"encoding/json"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/spinifex/utils"
	"github.com/nats-io/nats.go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDescribeInstanceTypes_SingleNode(t *testing.T) {
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
	output, err := DescribeInstanceTypes(input, nc, 1, "")

	require.NoError(t, err)
	require.NotNil(t, output)
	assert.Len(t, output.InstanceTypes, 2)
	assert.Equal(t, "t3.micro", *output.InstanceTypes[0].InstanceType)
	assert.Equal(t, "t3.small", *output.InstanceTypes[1].InstanceType)
}

func TestDescribeInstanceTypes_DeduplicatesAcrossNodes(t *testing.T) {
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
	output, err := DescribeInstanceTypes(input, nc, 2, "")

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
	output, err := DescribeInstanceTypes(input, nc, 2, "")

	require.NoError(t, err)
	require.NotNil(t, output)
	// Both slots should appear since capacity filter is enabled
	assert.Len(t, output.InstanceTypes, 2)
}

func TestDescribeInstanceTypes_CapacityFilterFalseDeduplicates(t *testing.T) {
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
	output, err := DescribeInstanceTypes(input, nc, 1, "")

	require.NoError(t, err)
	require.NotNil(t, output)
	assert.Len(t, output.InstanceTypes, 1)
}

func TestDescribeInstanceTypes_NoSubscribers(t *testing.T) {
	_, nc := startTestNATSServer(t)

	input := &ec2.DescribeInstanceTypesInput{}
	output, err := DescribeInstanceTypes(input, nc, 0, "")

	require.NoError(t, err)
	require.NotNil(t, output)
	assert.Empty(t, output.InstanceTypes)
}

func TestDescribeInstanceTypes_NodeReturnsError(t *testing.T) {
	_, nc := startTestNATSServer(t)

	nc.Subscribe("ec2.DescribeInstanceTypes", func(msg *nats.Msg) {
		errorPayload := utils.GenerateErrorPayload("InternalError")
		msg.Respond(errorPayload)
	})

	input := &ec2.DescribeInstanceTypesInput{}
	output, err := DescribeInstanceTypes(input, nc, 1, "")

	require.NoError(t, err)
	require.NotNil(t, output)
	assert.Empty(t, output.InstanceTypes)
}

func TestDescribeInstanceTypes_MalformedJSON(t *testing.T) {
	_, nc := startTestNATSServer(t)

	nc.Subscribe("ec2.DescribeInstanceTypes", func(msg *nats.Msg) {
		msg.Respond([]byte(`not valid json`))
	})

	input := &ec2.DescribeInstanceTypesInput{}
	output, err := DescribeInstanceTypes(input, nc, 1, "")

	require.NoError(t, err)
	require.NotNil(t, output)
	assert.Empty(t, output.InstanceTypes)
}

func TestDescribeInstanceTypes_NilInstanceTypeSkipped(t *testing.T) {
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
	output, err := DescribeInstanceTypes(input, nc, 1, "")

	require.NoError(t, err)
	require.NotNil(t, output)
	assert.Len(t, output.InstanceTypes, 1)
	assert.Equal(t, "t3.micro", *output.InstanceTypes[0].InstanceType)
}

func TestDescribeInstanceTypes_ClosedConnection(t *testing.T) {
	_, nc := startTestNATSServer(t)

	closedNC, err := nats.Connect(nc.ConnectedUrl())
	require.NoError(t, err)
	closedNC.Close()

	input := &ec2.DescribeInstanceTypesInput{}
	_, err = DescribeInstanceTypes(input, closedNC, 1, "")

	require.Error(t, err)
}
