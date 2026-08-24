package arn_test

import (
	"testing"

	"github.com/mulgadc/spinifex/spinifex/arn"
	"github.com/stretchr/testify/assert"
)

func TestEC2TypeForID(t *testing.T) {
	tests := []struct {
		resourceID string
		want       arn.EC2ResourceType
		wantOK     bool
	}{
		{"i-abc123", arn.EC2Instance, true},
		{"vol-abc123", arn.EC2Volume, true},
		{"ami-abc123", arn.EC2Image, true},
		{"snap-abc123", arn.EC2Snapshot, true},
		{"vpc-abc123", arn.EC2VPC, true},
		{"subnet-abc123", arn.EC2Subnet, true},
		{"sg-abc123", arn.EC2SecurityGroup, true},
		{"rtb-abc123", arn.EC2RouteTable, true},
		{"igw-abc123", arn.EC2InternetGateway, true},
		{"eigw-abc123", arn.EC2EgressOnlyInternetGateway, true},
		{"eni-abc123", arn.EC2NetworkInterface, true},
		{"eipalloc-abc123", arn.EC2ElasticIP, true},
		{"nat-abc123", arn.EC2NATGateway, true},
		{"key-abc123", arn.EC2KeyPair, true},
		{"pg-abc123", arn.EC2PlacementGroup, true},
		{"unknown-abc123", "", false},
		{"", "", false},
		{"i", "", false},
	}

	for _, tc := range tests {
		t.Run(tc.resourceID, func(t *testing.T) {
			got, ok := arn.EC2TypeForID(tc.resourceID)
			assert.Equal(t, tc.wantOK, ok)
			assert.Equal(t, tc.want, got)
		})
	}
}

func TestFormatEC2(t *testing.T) {
	assert.Equal(t,
		"arn:aws:ec2:us-east-1:123456789012:subnet/subnet-abc",
		arn.FormatEC2(arn.EC2Subnet, "us-east-1", "123456789012", "subnet-abc"))

	// A literal * is a value, not a pattern: it neither matches a scoped Deny
	// nor widens a grant.
	assert.Equal(t,
		"arn:aws:ec2:ap-southeast-2:123456789012:instance/*",
		arn.FormatEC2(arn.EC2Instance, "ap-southeast-2", "123456789012", "*"))
}
