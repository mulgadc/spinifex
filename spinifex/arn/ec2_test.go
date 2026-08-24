package arn

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestEC2TypeForID(t *testing.T) {
	tests := []struct {
		resourceID string
		want       EC2ResourceType
		wantOK     bool
	}{
		{"i-abc123", EC2Instance, true},
		{"vol-abc123", EC2Volume, true},
		{"ami-abc123", EC2Image, true},
		{"snap-abc123", EC2Snapshot, true},
		{"vpc-abc123", EC2VPC, true},
		{"subnet-abc123", EC2Subnet, true},
		{"sg-abc123", EC2SecurityGroup, true},
		{"rtb-abc123", EC2RouteTable, true},
		{"igw-abc123", EC2InternetGateway, true},
		{"eigw-abc123", EC2EgressOnlyInternetGateway, true},
		{"eni-abc123", EC2NetworkInterface, true},
		{"eipalloc-abc123", EC2ElasticIP, true},
		{"nat-abc123", EC2NATGateway, true},
		{"key-abc123", EC2KeyPair, true},
		{"pg-abc123", EC2PlacementGroup, true},
		{"unknown-abc123", "", false},
		{"", "", false},
		{"i", "", false},
	}

	for _, tc := range tests {
		t.Run(tc.resourceID, func(t *testing.T) {
			got, ok := EC2TypeForID(tc.resourceID)
			assert.Equal(t, tc.wantOK, ok)
			assert.Equal(t, tc.want, got)
		})
	}
}

func TestFormatEC2(t *testing.T) {
	assert.Equal(t,
		"arn:aws:ec2:us-east-1:123456789012:subnet/subnet-abc",
		FormatEC2(EC2Subnet, "us-east-1", "123456789012", "subnet-abc"))

	// A literal * is a value, not a pattern: it neither matches a scoped Deny
	// nor widens a grant.
	assert.Equal(t,
		"arn:aws:ec2:ap-southeast-2:123456789012:instance/*",
		FormatEC2(EC2Instance, "ap-southeast-2", "123456789012", "*"))
}
