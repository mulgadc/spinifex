package tgw

import (
	"encoding/binary"
	"fmt"
	"net/netip"
	"strings"
	"time"
)

const (
	KVBucketTransitGateways        = "spinifex-transit-gateways"
	KVBucketTransitGatewaysVersion = 1

	KVBucketAttachments        = "spinifex-transit-gateway-attachments"
	KVBucketAttachmentsVersion = 1

	KVBucketTerminal        = "spinifex-transit-gateways-terminal"
	KVBucketTerminalVersion = 1
)

// AWS states of a Transit Gateway and of a VPC attachment.
const (
	StatePending   = "pending"
	StateAvailable = "available"
	StateDeleting  = "deleting"
	StateDeleted   = "deleted"
	StateFailed    = "failed"
)

const (
	// attachmentWindow is AWS's documented visibility of a deleted or failed
	// attachment; transitGatewayWindow is the provisional one for a TGW.
	attachmentWindow     = 2 * time.Hour
	transitGatewayWindow = 2 * time.Hour
	// terminalBucketTTL backs up the sweep, which removes records at the window.
	terminalBucketTTL = 2 * max(attachmentWindow, transitGatewayWindow)
	// claimGrace is four 30-second request budgets: a claim or index key younger
	// than this may belong to a request still in flight and is never reclaimed.
	claimGrace = 2 * time.Minute

	// accountLimit is AWS's default quota of Transit Gateways per account.
	accountLimit = 5

	// moveAttempts bounds every re-read loop.
	moveAttempts = 5
)

// Link addresses are /30s carved from linkRange, each giving the VPC router .1
// and the Transit Gateway router .2 of its block.
var linkRange = netip.MustParsePrefix("169.254.64.0/18")

const linkSlots = 4096

// linkLocal is the range AWS refuses as a VPC CIDR; links live inside it.
var linkLocal = netip.MustParsePrefix("169.254.0.0/16")

// TransitGatewayOptions are the options a Transit Gateway stores and describes.
type TransitGatewayOptions struct {
	AmazonSideAsn                   int64  `json:"amazon_side_asn"`
	AutoAcceptSharedAttachments     string `json:"auto_accept_shared_attachments"`
	DnsSupport                      string `json:"dns_support"`
	VpnEcmpSupport                  string `json:"vpn_ecmp_support"`
	SecurityGroupReferencingSupport string `json:"security_group_referencing_support"`
}

// TransitGatewayRecord is one Transit Gateway, stored under <account>.<id> in
// the Transit Gateway bucket and moved to the terminal bucket once deleted.
type TransitGatewayRecord struct {
	ID                 string                `json:"id"`
	AccountID          string                `json:"account_id"`
	Description        string                `json:"description"`
	State              string                `json:"state"`
	Options            TransitGatewayOptions `json:"options"`
	Generation         int64                 `json:"generation"`
	RealizedGeneration int64                 `json:"realized_generation"`
	TerminalAt         time.Time             `json:"terminal_at,omitzero"`
	Tags               map[string]string     `json:"tags"`
	CreatedAt          time.Time             `json:"created_at"`
}

// AttachmentOptions are the options a VPC attachment stores and describes.
type AttachmentOptions struct {
	DnsSupport                      string `json:"dns_support"`
	SecurityGroupReferencingSupport string `json:"security_group_referencing_support"`
}

// AttachmentRecord is one VPC attachment, stored under <account>.<id> in the
// attachment bucket and moved to the terminal bucket once deleted or failed.
type AttachmentRecord struct {
	ID                 string            `json:"id"`
	AccountID          string            `json:"account_id"`
	TransitGatewayID   string            `json:"transit_gateway_id"`
	VpcID              string            `json:"vpc_id"`
	SubnetIDs          []string          `json:"subnet_ids"`
	Options            AttachmentOptions `json:"options"`
	State              string            `json:"state"`
	LinkPrefix         string            `json:"link_prefix,omitempty"`
	Generation         int64             `json:"generation"`
	RealizedGeneration int64             `json:"realized_generation"`
	TerminalAt         time.Time         `json:"terminal_at,omitzero"`
	Tags               map[string]string `json:"tags"`
	CreatedAt          time.Time         `json:"created_at"`
}

// isLive reports an attachment AWS counts as non-deleted.
func (r *AttachmentRecord) isLive() bool {
	return r.State == StatePending || r.State == StateAvailable || r.State == StateDeleting
}

// claim is the value of a pair., link. or slot. key: the attachment or
// Transit Gateway that holds it.
type claim struct {
	Owner     string    `json:"owner"`
	AccountID string    `json:"account_id"`
	ClaimedAt time.Time `json:"claimed_at"`
}

// indexEntry is the value of an acct., tgw., vpc. or subnet. key.
type indexEntry struct {
	AccountID string    `json:"account_id"`
	CreatedAt time.Time `json:"created_at"`
}

func recordKey(accountID, id string) string { return accountID + "." + id }

func acctKey(accountID, id string) string { return "acct." + accountID + "." + id }

func tgwIndexKey(tgwID, attID string) string { return "tgw." + tgwID + "." + attID }

func vpcIndexKey(vpcID, attID string) string { return "vpc." + vpcID + "." + attID }

func subnetIndexKey(subnetID, attID string) string { return "subnet." + subnetID + "." + attID }

func pairKey(tgwID, vpcID string) string { return "pair." + tgwID + "." + vpcID }

func linkKey(n int) string { return fmt.Sprintf("link.%d", n) }

func slotKey(accountID string, n int) string { return fmt.Sprintf("slot.%s.%d", accountID, n) }

// indexKinds are the key prefixes that are not records.
var indexKinds = map[string]bool{"acct": true, "tgw": true, "vpc": true, "subnet": true, "pair": true, "link": true, "slot": true}

// recordID returns the ID of a record key, or "" for an index or claim key.
func recordID(key string) (accountID, id string, ok bool) {
	kind, rest, found := strings.Cut(key, ".")
	if !found || indexKinds[kind] {
		return "", "", false
	}
	return kind, rest, true
}

// idFromIndexKey returns the trailing ID of an index key.
func idFromIndexKey(key string) string {
	_, id, ok := strings.CutLast(key, ".")
	if !ok {
		return ""
	}
	return id
}

// defaultRouteTableID names the Transit Gateway's default route table, which
// AWS returns in two option fields. It is derived, so it is never stored.
func defaultRouteTableID(tgwID string) string {
	return "tgw-rtb-" + strings.TrimPrefix(tgwID, "tgw-")
}

func isAttachmentID(id string) bool { return strings.HasPrefix(id, "tgw-attach-") }

func isTransitGatewayID(id string) bool {
	return strings.HasPrefix(id, "tgw-") && !isAttachmentID(id) && !strings.HasPrefix(id, "tgw-rtb-")
}

// linkPrefix returns slot n's /30.
func linkPrefix(n int) netip.Prefix {
	base := linkRange.Addr().As4()
	v := binary.BigEndian.Uint32(base[:]) + uint32(n)*4 //nolint:gosec // n is bounded by the slot count
	var out [4]byte
	binary.BigEndian.PutUint32(out[:], v)
	return netip.PrefixFrom(netip.AddrFrom4(out), 30)
}

// linkSlot returns the slot of a stored link prefix.
func linkSlot(prefix string) (int, bool) {
	p, err := netip.ParsePrefix(prefix)
	if err != nil || p.Bits() != 30 || !p.Addr().Is4() || !linkRange.Contains(p.Addr()) {
		return 0, false
	}
	a, base := p.Addr().As4(), linkRange.Addr().As4()
	return int(binary.BigEndian.Uint32(a[:])-binary.BigEndian.Uint32(base[:])) / 4, true
}

// LinkAddresses returns the VPC and Transit Gateway router addresses of a
// stored link prefix.
func LinkAddresses(prefix string) (vpcRouter, tgwRouter netip.Addr, err error) {
	p, err := netip.ParsePrefix(prefix)
	if err != nil {
		return netip.Addr{}, netip.Addr{}, fmt.Errorf("parse link prefix %q: %w", prefix, err)
	}
	if p.Bits() != 30 || !p.Addr().Is4() || !linkRange.Contains(p.Addr()) {
		return netip.Addr{}, netip.Addr{}, fmt.Errorf("link prefix %q is not a /30 in %s", prefix, linkRange)
	}
	vpcRouter = p.Masked().Addr().Next()
	return vpcRouter, vpcRouter.Next(), nil
}

// overlapsLinkLocal reports a CIDR that overlaps 169.254.0.0/16.
func overlapsLinkLocal(cidr string) bool {
	p, err := netip.ParsePrefix(cidr)
	if err != nil {
		return false
	}
	return p.Overlaps(linkLocal)
}
