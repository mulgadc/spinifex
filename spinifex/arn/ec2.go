// Package arn builds AWS resource ARNs. It is a leaf: it imports nothing from
// spinifex, so gateway-side and handler-side code can share one construction of
// an ARN rather than each keeping its own.
package arn

import (
	"fmt"
	"strings"
)

// EC2ResourceType is the resource-type segment of an arn:aws:ec2 ARN.
type EC2ResourceType string

// The types an EC2 resource id can be recognised as by prefix. These are the
// types the tag store records against a resource id.
const (
	EC2Instance                  EC2ResourceType = "instance"
	EC2Volume                    EC2ResourceType = "volume"
	EC2Image                     EC2ResourceType = "image"
	EC2Snapshot                  EC2ResourceType = "snapshot"
	EC2VPC                       EC2ResourceType = "vpc"
	EC2Subnet                    EC2ResourceType = "subnet"
	EC2SecurityGroup             EC2ResourceType = "security-group"
	EC2RouteTable                EC2ResourceType = "route-table"
	EC2InternetGateway           EC2ResourceType = "internet-gateway"
	EC2EgressOnlyInternetGateway EC2ResourceType = "egress-only-internet-gateway"
	EC2NetworkInterface          EC2ResourceType = "network-interface"
	EC2ElasticIP                 EC2ResourceType = "elastic-ip"
	EC2NATGateway                EC2ResourceType = "natgateway"
	EC2KeyPair                   EC2ResourceType = "key-pair"
	EC2PlacementGroup            EC2ResourceType = "placement-group"
)

// Types that carry no id prefix of their own, so they are named by a parameter
// rather than recognised from an id.
const (
	EC2LaunchTemplate       EC2ResourceType = "launch-template"
	EC2CapacityReservation  EC2ResourceType = "capacity-reservation"
	EC2SpotInstancesRequest EC2ResourceType = "spot-instances-request"
)

// Matched in order. No prefix here is a prefix of another, so the order is the
// tag store's historical one rather than a disambiguation.
var ec2IDPrefixes = []struct {
	prefix string
	kind   EC2ResourceType
}{
	{"i-", EC2Instance},
	{"vol-", EC2Volume},
	{"ami-", EC2Image},
	{"snap-", EC2Snapshot},
	{"vpc-", EC2VPC},
	{"subnet-", EC2Subnet},
	{"sg-", EC2SecurityGroup},
	{"rtb-", EC2RouteTable},
	{"igw-", EC2InternetGateway},
	{"eigw-", EC2EgressOnlyInternetGateway},
	{"eni-", EC2NetworkInterface},
	{"eipalloc-", EC2ElasticIP},
	{"nat-", EC2NATGateway},
	{"key-", EC2KeyPair},
	{"pg-", EC2PlacementGroup},
}

// EC2TypeForID maps an EC2 resource id to its ARN type by prefix. ok is false
// for an unrecognised prefix, which has no correct ARN: a sentinel type that
// still looks like a valid ARN segment is how a wrong ARN gets built.
func EC2TypeForID(id string) (EC2ResourceType, bool) {
	for _, p := range ec2IDPrefixes {
		if strings.HasPrefix(id, p.prefix) {
			return p.kind, true
		}
	}
	return "", false
}

// FormatEC2 builds arn:aws:ec2:<region>:<account>:<type>/<id>.
func FormatEC2(kind EC2ResourceType, region, accountID, id string) string {
	return fmt.Sprintf("arn:aws:ec2:%s:%s:%s/%s", region, accountID, kind, id)
}
