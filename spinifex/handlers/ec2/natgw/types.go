package handlers_ec2_natgw

import "time"

const (
	KVBucketNatGateways        = "spinifex-vpc-nat-gateways"
	KVBucketNatGatewaysVersion = 1

	KVBucketDeletedNatGateways        = "spinifex-vpc-deleted-nat-gateways"
	KVBucketDeletedNatGatewaysVersion = 1
)

// NatGatewayRecord represents a stored NAT Gateway.
type NatGatewayRecord struct {
	NatGatewayId string            `json:"nat_gateway_id"`
	VpcId        string            `json:"vpc_id"`
	SubnetId     string            `json:"subnet_id"`     // public subnet where NAT GW lives
	AllocationId string            `json:"allocation_id"` // EIP allocation
	PublicIp     string            `json:"public_ip"`     // EIP address
	State        string            `json:"state"`         // pending, available, deleting, deleted, failed
	AccountID    string            `json:"account_id"`
	Tags         map[string]string `json:"tags"`
	CreatedAt    time.Time         `json:"created_at"`
}
