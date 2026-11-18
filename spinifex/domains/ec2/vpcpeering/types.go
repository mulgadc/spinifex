package vpcpeering

import (
	"encoding/binary"
	"fmt"
	"net/netip"
	"strings"
	"time"
)

const (
	KVBucketPeeringConnections        = "spinifex-vpc-peering-connections"
	KVBucketPeeringConnectionsVersion = 1

	KVBucketPeeringConnectionsTerminal        = "spinifex-vpc-peering-connections-terminal"
	KVBucketPeeringConnectionsTerminalVersion = 1
)

// AWS state codes for a peering connection.
const (
	StateInitiatingRequest = "initiating-request"
	StatePendingAcceptance = "pending-acceptance"
	StateProvisioning      = "provisioning"
	StateActive            = "active"
	StateFailed            = "failed"
	StateRejected          = "rejected"
	StateExpired           = "expired"
	StateDeleted           = "deleted"
	StateDeleting          = "deleting"
)

const (
	// requestLifetime is how long a request waits for acceptance.
	requestLifetime = 7 * 24 * time.Hour
	// terminalShortWindow and terminalLongWindow are AWS's visibility windows
	// for a connection that has left the live lifecycle.
	terminalShortWindow = 2 * time.Hour
	terminalLongWindow  = 48 * time.Hour
	// terminalBucketTTL keeps a terminal record for the longest window.
	terminalBucketTTL = terminalLongWindow
	// claimGrace is four 30-second request budgets: a claim or index key younger
	// than this may belong to a request still in flight and is never reclaimed.
	claimGrace = 2 * time.Minute

	// outstandingLimit caps pending requests a VPC has made; perVpcLimit caps
	// pending plus provisioning plus active connections on a VPC.
	outstandingLimit = 25
	perVpcLimit      = 50
)

// Link addresses are /30s carved from linkRange: linkSlots of them, each
// giving the requester router .1 and the accepter router .2 of its block.
var linkRange = netip.MustParsePrefix("169.254.192.0/18")

const linkSlots = 4096

// PeeringOptions is one side's options as AWS stores and describes them.
type PeeringOptions struct {
	AllowDnsResolutionFromRemoteVpc            bool `json:"allow_dns_resolution_from_remote_vpc"`
	AllowEgressFromLocalClassicLinkToRemoteVpc bool `json:"allow_egress_from_local_classic_link_to_remote_vpc"`
	AllowEgressFromLocalVpcToRemoteClassicLink bool `json:"allow_egress_from_local_vpc_to_remote_classic_link"`
}

// PeeringRecord is one connection, stored under pcx.<id> in the live bucket
// and moved to the terminal bucket once it leaves the live lifecycle.
type PeeringRecord struct {
	ID                 string                       `json:"id"`
	RequesterAccountID string                       `json:"requester_account_id"`
	RequesterVpcID     string                       `json:"requester_vpc_id"`
	RequesterCIDR      string                       `json:"requester_cidr"`
	AccepterAccountID  string                       `json:"accepter_account_id"`
	AccepterVpcID      string                       `json:"accepter_vpc_id"`
	AccepterCIDR       string                       `json:"accepter_cidr"`
	Region             string                       `json:"region"`
	Status             string                       `json:"status"`
	StatusMessage      string                       `json:"status_message"`
	ExpirationTime     time.Time                    `json:"expiration_time,omitzero"`
	RequesterOptions   PeeringOptions               `json:"requester_options"`
	AccepterOptions    PeeringOptions               `json:"accepter_options"`
	LinkPrefix         string                       `json:"link_prefix,omitempty"`
	Generation         int64                        `json:"generation"`
	RealizedGeneration int64                        `json:"realized_generation"`
	CleanupPending     bool                         `json:"cleanup_pending,omitempty"`
	TerminalAt         time.Time                    `json:"terminal_at,omitzero"`
	TerminatedBy       string                       `json:"terminated_by,omitempty"`
	Tags               map[string]map[string]string `json:"tags"`
	CreatedAt          time.Time                    `json:"created_at"`
}

// isTerminalStatus reports a status that has left the live lifecycle. A
// deleted record still awaiting cleanup is not yet terminal.
func (r *PeeringRecord) isTerminalStatus() bool {
	switch r.Status {
	case StateFailed, StateRejected, StateExpired:
		return true
	case StateDeleted:
		return !r.CleanupPending
	}
	return false
}

// involves reports whether accountID is one of the connection's owners.
func (r *PeeringRecord) involves(accountID string) bool {
	return r.RequesterAccountID == accountID || r.AccepterAccountID == accountID
}

// accounts lists the owning accounts once each.
func (r *PeeringRecord) accounts() []string {
	if r.RequesterAccountID == r.AccepterAccountID {
		return []string{r.RequesterAccountID}
	}
	return []string{r.RequesterAccountID, r.AccepterAccountID}
}

// claim is the value of a req., pair. or link. key.
type claim struct {
	PcxID     string    `json:"pcx_id"`
	ClaimedAt time.Time `json:"claimed_at"`
}

// indexEntry is the value of an acct. or vpc. key.
type indexEntry struct {
	CreatedAt time.Time `json:"created_at"`
}

func recordKey(id string) string { return "pcx." + id }

func acctKey(accountID, id string) string { return "acct." + accountID + "." + id }

func vpcKey(vpcID, id string) string { return "vpc." + vpcID + "." + id }

func reqKey(requesterVpcID, accepterVpcID string) string {
	return "req." + requesterVpcID + "." + accepterVpcID
}

// pairKey orders the two VPCs, so a reversed connection names the same key.
func pairKey(a, b string) string {
	if b < a {
		a, b = b, a
	}
	return "pair." + a + "." + b
}

func linkKey(n int) string { return fmt.Sprintf("link.%d", n) }

// idFromIndexKey returns the trailing pcx ID of an acct. or vpc. key.
func idFromIndexKey(key string) string {
	_, id, ok := strings.CutLast(key, ".")
	if !ok {
		return ""
	}
	return id
}

// linkPrefix returns slot n's /30.
func linkPrefix(n int) netip.Prefix {
	base := linkRange.Addr().As4()
	v := binary.BigEndian.Uint32(base[:]) + uint32(n)*4 //nolint:gosec // n is bounded by linkSlots
	var out [4]byte
	binary.BigEndian.PutUint32(out[:], v)
	return netip.PrefixFrom(netip.AddrFrom4(out), 30)
}

// LinkAddresses returns the requester and accepter router addresses of a
// stored link prefix.
func LinkAddresses(prefix string) (requester, accepter netip.Addr, err error) {
	p, err := netip.ParsePrefix(prefix)
	if err != nil {
		return netip.Addr{}, netip.Addr{}, fmt.Errorf("parse link prefix %q: %w", prefix, err)
	}
	if p.Bits() != 30 || !p.Addr().Is4() || !linkRange.Contains(p.Addr()) {
		return netip.Addr{}, netip.Addr{}, fmt.Errorf("link prefix %q is not a /30 in %s", prefix, linkRange)
	}
	requester = p.Masked().Addr().Next()
	return requester, requester.Next(), nil
}
