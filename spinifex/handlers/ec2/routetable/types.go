package handlers_ec2_routetable

import "time"

const (
	KVBucketRouteTables        = "spinifex-vpc-route-tables"
	KVBucketRouteTablesVersion = 1
)

// RouteTableRecord represents a stored Route Table.
type RouteTableRecord struct {
	RouteTableId string              `json:"route_table_id"`
	VpcId        string              `json:"vpc_id"`
	AccountID    string              `json:"account_id"`
	IsMain       bool                `json:"is_main"`
	Routes       []RouteRecord       `json:"routes"`
	Associations []AssociationRecord `json:"associations"`
	Tags         map[string]string   `json:"tags"`
	CreatedAt    time.Time           `json:"created_at"`
}

// RouteRecord represents a single route in a route table.
type RouteRecord struct {
	DestinationCidrBlock string `json:"destination_cidr_block"`
	GatewayId            string `json:"gateway_id,omitempty"`     // "local" or "igw-xxx"
	NatGatewayId         string `json:"nat_gateway_id,omitempty"` // "nat-xxx" (future)
	State                string `json:"state"`                    // "active", "blackhole"
	Origin               string `json:"origin"`                   // "CreateRouteTable", "CreateRoute"
}

// AssociationRecord represents a route table ↔ subnet association.
type AssociationRecord struct {
	AssociationId string `json:"association_id"`
	SubnetId      string `json:"subnet_id,omitempty"` // empty for main table VPC association
	Main          bool   `json:"main"`
}
