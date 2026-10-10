package natgw

import (
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// --- recordToEC2 ---

func TestRecordToEC2_Basic(t *testing.T) {
	now := time.Now()
	record := &NatGatewayRecord{
		NatGatewayId: "nat-abc",
		VpcId:        "vpc-1",
		SubnetId:     "subnet-1",
		State:        "available",
		AccountID:    "111222333444",
		CreatedAt:    now,
	}
	ngw := recordToEC2(record)
	assert.Equal(t, "nat-abc", *ngw.NatGatewayId)
	assert.Equal(t, "vpc-1", *ngw.VpcId)
	assert.Equal(t, "subnet-1", *ngw.SubnetId)
	assert.Equal(t, "available", *ngw.State)
	assert.Equal(t, "public", *ngw.ConnectivityType)
	assert.Equal(t, now, *ngw.CreateTime)
	assert.Empty(t, ngw.NatGatewayAddresses, "no addresses when PublicIp empty")
	assert.Empty(t, ngw.Tags)
}

func TestRecordToEC2_WithPublicIp(t *testing.T) {
	record := &NatGatewayRecord{
		NatGatewayId: "nat-1",
		VpcId:        "vpc-1",
		SubnetId:     "subnet-1",
		AllocationId: "eipalloc-abc",
		PublicIp:     "54.1.2.3",
		State:        "available",
		CreatedAt:    time.Now(),
	}
	ngw := recordToEC2(record)
	require.Len(t, ngw.NatGatewayAddresses, 1)
	assert.Equal(t, "eipalloc-abc", *ngw.NatGatewayAddresses[0].AllocationId)
	assert.Equal(t, "54.1.2.3", *ngw.NatGatewayAddresses[0].PublicIp)
}

func TestRecordToEC2_WithTags(t *testing.T) {
	record := &NatGatewayRecord{
		NatGatewayId: "nat-1",
		VpcId:        "vpc-1",
		SubnetId:     "subnet-1",
		State:        "available",
		Tags:         map[string]string{"Name": "my-nat", "env": "test"},
		CreatedAt:    time.Now(),
	}
	ngw := recordToEC2(record)
	require.Len(t, ngw.Tags, 2)

	tagMap := make(map[string]string)
	for _, tag := range ngw.Tags {
		tagMap[aws.StringValue(tag.Key)] = aws.StringValue(tag.Value)
	}
	assert.Equal(t, "my-nat", tagMap["Name"])
	assert.Equal(t, "test", tagMap["env"])
}
