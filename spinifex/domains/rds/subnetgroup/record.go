// Package subnetgroup owns the RDS DB subnet group: its durable record, the
// validation of its member subnets, and the in-use guard on its delete.
package subnetgroup

import "time"

const prefix = "db-subnet-groups/"

// Prefix returns the per-account key prefix under which each DB subnet group record lives.
func Prefix() string {
	return prefix
}

// Key returns the per-account KV key of a DB subnet group's record.
func Key(name string) string {
	return prefix + name
}

// Record is the db-subnet-groups/{name} record. The subnet list is stored verbatim rather than
// reduced to a placement, so when V2 makes AZs real the group needs no migration — only the code
// that chooses among its subnets changes.
type Record struct {
	Name        string `json:"name"`
	AccountID   string `json:"accountId"`
	Description string `json:"description"`
	// Every subnet the customer supplied, in request order, each with the AZ
	// recorded on the subnet itself rather than a hardcoded zone.
	Subnets []Subnet `json:"subnets"`
	// The one VPC they all share, which is what makes the group usable for a
	// placement at all.
	VpcID string `json:"vpcId"`

	Tags map[string]string `json:"tags,omitempty"`

	CreatedAt time.Time `json:"createdAt"`
	UpdatedAt time.Time `json:"updatedAt"`
}

// Subnet is one subnet of a DB subnet group, with the AZ recorded on the subnet itself;
// AvailabilityZone is empty when the subnet carries none.
type Subnet struct {
	SubnetID         string `json:"subnetId"`
	AvailabilityZone string `json:"availabilityZone,omitempty"`
}

func (r *Record) GetTags() map[string]string { return r.Tags }

func (r *Record) SetTags(tags map[string]string) { r.Tags = tags }
