package handlers_ec2_eip

import "time"

const (
	KVBucketEIPs        = "spinifex-vpc-elastic-ips"
	KVBucketEIPsVersion = 1
)

// EIPRecord represents a stored Elastic IP address allocation.
type EIPRecord struct {
	AllocationId  string            `json:"allocation_id"`
	PublicIp      string            `json:"public_ip"`
	PoolName      string            `json:"pool_name"`
	AssociationId string            `json:"association_id,omitempty"`
	ENIId         string            `json:"eni_id,omitempty"`
	InstanceId    string            `json:"instance_id,omitempty"`
	PrivateIp     string            `json:"private_ip,omitempty"`
	VpcId         string            `json:"vpc_id,omitempty"`
	State         string            `json:"state"` // "allocated", "associated"
	Tags          map[string]string `json:"tags"`
	CreatedAt     time.Time         `json:"created_at"`
}
