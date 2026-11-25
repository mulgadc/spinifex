package vpcendpoint

import (
	"slices"
	"strings"
	"time"

	"github.com/mulgadc/spinifex/spinifex/foundation/state/kvstore"
)

const (
	KVBucketVpcEndpoints        = "spinifex-vpc-endpoints"
	KVBucketVpcEndpointsVersion = 1
)

// Stored states. Only available is ever rendered: AWS shows a gateway endpoint
// as available from the create response on, and absent once deleted.
const (
	StatePending   = "pending"
	StateAvailable = "available"
	StateDeleting  = "deleting"
)

// TypeGateway is the only endpoint type offered.
const TypeGateway = "Gateway"

// serviceKey is the claim suffix for the S3 service, the only one offered.
const serviceKey = "s3"

const (
	// claimGrace is four 30-second request budgets: a claim or record younger
	// than this may belong to a request still in flight and is never reclaimed.
	claimGrace = 2 * time.Minute

	// accountLimit is AWS's Regional gateway endpoint quota, counted per
	// account. AWS's 255 per VPC cannot be reached under it.
	accountLimit = 20

	// maxPolicyLength is AWS's limit on an endpoint policy document.
	maxPolicyLength = 20480

	// moveAttempts bounds each re-read loop after a lost revision race.
	moveAttempts = 5
)

// EndpointRecord is one endpoint, keyed {account}.{id} in the bucket.
type EndpointRecord struct {
	ID          string `json:"id"`
	AccountID   string `json:"account_id"`
	VpcID       string `json:"vpc_id"`
	ServiceName string `json:"service_name"`
	Type        string `json:"type"`
	// RouteTableIds is sorted. A table counts only while the association is
	// effective (see Store.effective), so a listed table may not render.
	RouteTableIds []string `json:"route_table_ids"`
	// PolicyDocument is the stored custom policy; "" is the default.
	PolicyDocument  string            `json:"policy_document,omitempty"`
	DnsRecordIpType string            `json:"dns_record_ip_type,omitempty"`
	State           string            `json:"state"`
	Generation      int64             `json:"generation"`
	Tags            map[string]string `json:"tags,omitempty"`
	CreatedAt       time.Time         `json:"created_at"`
}

// claim is the value of an rtb.<table>.<service> key: the one endpoint whose
// route for the service the table may carry.
type claim struct {
	VpceID    string    `json:"vpce_id"`
	AccountID string    `json:"account_id"`
	ClaimedAt time.Time `json:"claimed_at"`
}

func recordKey(accountID, id string) string { return kvstore.AccountKey(accountID, id) }

func claimKey(rtbID string) string { return "rtb." + rtbID + "." + serviceKey }

// rtbFromClaimKey returns the table a claim key names.
func rtbFromClaimKey(key string) (string, bool) {
	rest, ok := strings.CutPrefix(key, "rtb.")
	if !ok {
		return "", false
	}
	rtb, svc, ok := strings.Cut(rest, ".")
	return rtb, ok && svc == serviceKey
}

// lists reports whether the record names table.
func (r *EndpointRecord) lists(table string) bool { return slices.Contains(r.RouteTableIds, table) }
