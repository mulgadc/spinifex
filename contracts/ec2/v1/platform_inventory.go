package ec2v1

import "time"

// PlatformInventorySubject is the request/reply subject on which a daemon in
// the spinifex-workers queue group reports one account's key pairs, placement
// groups, launch templates and their tag-index entries. It never writes.
const PlatformInventorySubject = "ec2.v1.PlatformInventory"

// Kinds reported by a platform inventory. The tag index is its own kind
// because it is a separate store with its own completeness.
const (
	PlatformKindKeyPair        = "key-pair"
	PlatformKindPlacementGroup = "placement-group"
	PlatformKindLaunchTemplate = "launch-template"
	PlatformKindTagIndex       = "tag-index"
)

// PlatformKinds lists every kind a complete reply reports, in reply order.
func PlatformKinds() []string {
	return []string{PlatformKindKeyPair, PlatformKindPlacementGroup, PlatformKindLaunchTemplate, PlatformKindTagIndex}
}

// Per-kind statuses. Only PlatformStatusObservedClean says a kind's stores
// were read completely and held nothing, and only at scan time: writers are
// not fenced, so it is a snapshot and never evidence of absence.
const (
	PlatformStatusObservedClean  = "observed-clean-at-scan-time"
	PlatformStatusRecordsFound   = "records-found"
	PlatformStatusScanIncomplete = "scan-incomplete"
)

// Item and count dispositions. A store failure is reported once per kind as
// PlatformStatusScanIncomplete rather than per record.
const (
	PlatformDispositionLive           = "live"
	PlatformDispositionResidue        = "residue"
	PlatformDispositionUnreadable     = "unreadable"
	PlatformDispositionDependencyHeld = "dependency-held"
)

// Record classes that are reported only as aggregate counts.
const (
	PlatformClassPublicKeyWithoutMetadata  = "public-key-object-without-metadata"
	PlatformClassPublicKeyUnresolved       = "public-key-object-unresolved"
	PlatformClassUnreadableRecord          = "unreadable-record"
	PlatformClassVersionBodyWithoutHeader  = "version-body-without-header"
	PlatformClassNameIndexWithoutHeader    = "name-index-without-header"
	PlatformClassUnrecognisedTemplateEntry = "unrecognised-template-entry"
)

// Generic reasons a kind is PlatformStatusScanIncomplete. They are categories,
// never store error text, which can quote stored content.
const (
	PlatformReasonStoreListFailed        = "store-list-failed"
	PlatformReasonRecordReadFailed       = "record-read-failed"
	PlatformReasonOwnerRecordsUnresolved = "owner-records-unresolved"
	PlatformReasonNoResponder            = "no-responder"
	PlatformReasonTimeout                = "timeout"
	PlatformReasonTransportFailed        = "transport-failed"
	PlatformReasonErrorReply             = "error-reply"
	PlatformReasonInvalidReply           = "invalid-reply"
	PlatformReasonKindNotReported        = "kind-not-reported"
)

// PlatformInventoryRequest asks for one account's platform inventory.
// CorrelationRef is an opaque caller reference echoed for logs; it grants
// nothing and is never a fence or authority to write.
type PlatformInventoryRequest struct {
	AccountID      string `json:"account_id"`
	CorrelationRef string `json:"correlation_ref,omitempty"`
}

// PlatformInventoryReply carries per-kind status, the identities a direct
// caller of the per-kind Describe subjects can already read, and counts for
// every record class those subjects do not expose. It carries no record
// keys, names, tags, key material, template data or record bodies.
type PlatformInventoryReply struct {
	AccountID      string                `json:"account_id"`
	CorrelationRef string                `json:"correlation_ref,omitempty"`
	Kinds          []PlatformKindReport  `json:"kinds"`
	Items          []PlatformItem        `json:"items,omitempty"`
	Counts         []PlatformRecordCount `json:"counts,omitempty"`
}

// PlatformKindReport is one kind's scan status and when its scan ran.
type PlatformKindReport struct {
	Kind              string    `json:"kind"`
	Status            string    `json:"status"`
	IncompleteReasons []string  `json:"incomplete_reasons,omitempty"`
	ScanStartedAt     time.Time `json:"scan_started_at"`
	ScanEndedAt       time.Time `json:"scan_ended_at"`
}

// PlatformItem is one resource by its AWS resource ID. MemberInstanceIDs is
// set only for a dependency-held placement group.
type PlatformItem struct {
	ResourceID        string   `json:"resource_id"`
	Kind              string   `json:"kind"`
	Disposition       string   `json:"disposition"`
	MemberInstanceIDs []string `json:"member_instance_ids,omitempty"`
}

// PlatformRecordCount is the number of records of one class in one kind.
type PlatformRecordCount struct {
	Kind        string `json:"kind"`
	Disposition string `json:"disposition"`
	Class       string `json:"class"`
	Count       int    `json:"count"`
}

// IncompletePlatformInventory is the reply a consumer substitutes when no
// usable answer arrived: every kind is scan-incomplete for reason.
func IncompletePlatformInventory(accountID, correlationRef, reason string) PlatformInventoryReply {
	reply := PlatformInventoryReply{AccountID: accountID, CorrelationRef: correlationRef}
	for _, kind := range PlatformKinds() {
		reply.Kinds = append(reply.Kinds, PlatformKindReport{
			Kind: kind, Status: PlatformStatusScanIncomplete, IncompleteReasons: []string{reason},
		})
	}
	return reply
}
