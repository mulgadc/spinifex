package accountcleanup

import "time"

// Request names the account to clean. CorrelationRef is an opaque caller
// reference echoed for logs; it grants nothing and is not a fence.
type Request struct {
	AccountID      string `json:"account_id"`
	CorrelationRef string `json:"correlation_ref,omitempty"`
}

// Result is what the owner stores hold after one run. Kinds reuse the ec2.v1
// platform kind and status vocabulary; Items and Counts carry resource IDs,
// fixed reasons and counts only, never names, tags, key material or data.
type Result struct {
	AccountID      string       `json:"account_id"`
	CorrelationRef string       `json:"correlation_ref,omitempty"`
	Kinds          []KindReport `json:"kinds"`
	Items          []Item       `json:"items,omitempty"`
	Counts         []Count      `json:"counts,omitempty"`
}

// KindReport is one kind's status after its re-scan. Clean means the re-scan
// read every store completely and found nothing; writers are not fenced, so
// it is a snapshot, not evidence of absence.
type KindReport struct {
	Kind              string    `json:"kind"`
	Status            string    `json:"status"`
	IncompleteReasons []string  `json:"incomplete_reasons,omitempty"`
	StartedAt         time.Time `json:"started_at"`
	EndedAt           time.Time `json:"ended_at"`
}

// Item is one resource by its AWS resource ID. MemberCount is set only for a
// placement group blocked by members; instance and request IDs are never reported.
type Item struct {
	ResourceID  string `json:"resource_id"`
	Kind        string `json:"kind"`
	Disposition string `json:"disposition"`
	Reason      string `json:"reason,omitempty"`
	MemberCount int    `json:"member_count,omitempty"`
}

// Count is the number of records of one class with one disposition, for
// records that have no public identity.
type Count struct {
	Kind        string `json:"kind"`
	Class       string `json:"class"`
	Disposition string `json:"disposition"`
	Reason      string `json:"reason,omitempty"`
	Count       int    `json:"count"`
}

// Dispositions. Only a complete owner re-scan that no longer finds a record
// makes it deleted.
const (
	DispositionDeleted = "deleted"
	DispositionBlocked = "blocked"
	DispositionFailed  = "failed"
	DispositionResidue = "residue"
)

// Reasons are fixed categories, never store error text.
const (
	// ReasonInstanceMembers: a placement group still names instances.
	ReasonInstanceMembers = "instance-members"
	// ReasonOwnerDeleteFailed: the owner delete hit a store failure.
	ReasonOwnerDeleteFailed = "owner-delete-failed"
	// ReasonStillPresent: the owner reported a delete but the re-scan found it.
	ReasonStillPresent = "still-present"
	// ReasonAbsenceUnproven: the re-scan failed, so absence is not shown.
	ReasonAbsenceUnproven = "absence-unproven"
	// ReasonUnreadableRecord: a record the owner cannot read is never deleted.
	ReasonUnreadableRecord = "unreadable-record"
	// ReasonUnrecognisedRecord: an entry matching no owner record shape.
	ReasonUnrecognisedRecord = "unrecognised-record"
	// ReasonOwnerRecordsUnresolved: the record may belong to an unreadable one.
	ReasonOwnerRecordsUnresolved = "owner-records-unresolved"
	// ReasonRecentlyWritten: residue younger than the settle window, which may
	// be a create still in flight.
	ReasonRecentlyWritten = "recently-written"
	// ReasonAppearedDuringCleanup: a record the run did not see until its re-scan.
	ReasonAppearedDuringCleanup = "appeared-during-cleanup"
	// ReasonInstanceReference: a non-terminated instance record names it.
	ReasonInstanceReference = "instance-reference"
	// ReasonSpotRequestReference: an open or active spot request names it.
	ReasonSpotRequestReference = "spot-request-reference"
	// ReasonLaunchTemplateReference: a version of a live launch template names it.
	ReasonLaunchTemplateReference = "launch-template-reference"
	// ReasonReferenceUnknown: a record EC2 cannot read may name it, so it is
	// never deleted by account cleanup.
	ReasonReferenceUnknown = "reference-unknown"
	// ReasonReferenceCheckFailed: an instance, spot or launch-template reference
	// check did not complete, so nothing it guards was deleted. Also a kind
	// incomplete reason.
	ReasonReferenceCheckFailed = "reference-check-failed"
)
