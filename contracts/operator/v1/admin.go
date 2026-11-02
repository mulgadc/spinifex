package operatorv1

import "slices"

// adminMethodNames is the callable /admin/<Method> list. The gateway router
// builds its accept set from it, so a name cannot be served without being listed.
var adminMethodNames = []string{
	"CreateAccount",
	"DeleteAccount",
	"DescribeAccountDeletion",
	"GetAccountQuota",
	"ListAccounts",
	"PutAccountQuota",
}

// AdminMethodNames returns the callable /admin/<Method> names in sorted order.
// Tooling that mints a credential grants these by name, so the list has to be
// the one the router serves rather than a copy that can drift out of step.
func AdminMethodNames() []string {
	return slices.Sorted(slices.Values(adminMethodNames))
}

// Deletion job states.
const (
	DeletionStateRunning   = "RUNNING"
	DeletionStateCompleted = "COMPLETED"
	DeletionStateFailed    = "FAILED"

	// DeletionStateExercised ends a non-finalizing run: the account is left
	// TERMINATING and a later DeleteAccount takes the same operation over.
	DeletionStateExercised = "EXERCISED"
)

// CreateAccountRequest is the /admin/CreateAccount request body.
type CreateAccountRequest struct {
	Name        string `json:"name"`
	ClientToken string `json:"clientToken"`
	Source      string `json:"source,omitempty"`
}

// CreateAccountResponse is the /admin/CreateAccount success body. It is stored
// verbatim (with the secret encrypted) so a replay returns an identical answer.
type CreateAccountResponse struct {
	AccountID       string `json:"accountId"`
	AccountName     string `json:"accountName"`
	AdminUser       string `json:"adminUser"`
	AccessKeyID     string `json:"accessKeyId"`
	SecretAccessKey string `json:"secretAccessKey"`
	DefaultVpcID    string `json:"defaultVpcId"`
}

// DeleteAccountRequest is the /admin/DeleteAccount request body.
type DeleteAccountRequest struct {
	AccountID   string `json:"accountId"`
	AccountName string `json:"accountName"`
	ClientToken string `json:"clientToken"`
	Force       bool   `json:"force,omitempty"`
	DryRun      bool   `json:"dryRun,omitempty"`

	// NonFinalizing quiesces the account, cleans the quiesced EC2 platform
	// kinds and stops. No legacy stage runs and the account record stays.
	NonFinalizing bool `json:"nonFinalizing,omitempty"`
}

// DescribeAccountDeletionRequest is the /admin/DescribeAccountDeletion body.
type DescribeAccountDeletionRequest struct {
	AccountID string `json:"accountId"`
}

// AccountSummary is one row of /admin/ListAccounts. It carries no credential
// material: the caller can already create and delete accounts, but that is no
// reason for a listing to hand out keys.
type AccountSummary struct {
	AccountID   string `json:"accountId"`
	AccountName string `json:"accountName"`
	Status      string `json:"status"`
	CreatedAt   string `json:"createdAt,omitempty"`
}

// ListAccountsResponse is the /admin/ListAccounts success body.
type ListAccountsResponse struct {
	Accounts []AccountSummary `json:"accounts"`
	Count    int              `json:"count"`
}

// IAMQuiescence is the coordinator's recorded view of IAM quiescence for one
// fence. Recording only: it admits nothing. QuiescedScope is filled only for a
// non-finalizing run, and holds as of EvaluatedAt: a write after that is not caught.
type IAMQuiescence struct {
	OperationID   string            `json:"operationId"`
	Fence         uint64            `json:"fence"`
	Generation    uint64            `json:"generation"`
	QuiescedScope []string          `json:"quiescedScope"`
	Unresolved    []UnresolvedScope `json:"unresolved"`
	Consumers     []ConsumerBasis   `json:"consumers"`
	Writers       []AccountedWriter `json:"writers"`
	Inventory     []InventoryStatus `json:"inventory"`
	EvaluatedAt   string            `json:"evaluatedAt"`
}

// UnresolvedScope names a scope or writer that blocks quiescence, with a fixed
// reason. It is never a timeout.
type UnresolvedScope struct {
	Scope  string `json:"scope"`
	Class  string `json:"class"`
	Reason string `json:"reason"`
}

// ConsumerBasis cites an enforcement consumer and the accepted contract that
// lets it count as having applied the generation.
type ConsumerBasis struct {
	Consumer string `json:"consumer"`
	Basis    string `json:"basis"`
}

// AccountedWriter is a writer that cannot be fenced and is covered by the
// owner's inventory instead. Ref names the tracked work that closes the gap.
type AccountedWriter struct {
	Writer string `json:"writer"`
	Basis  string `json:"basis"`
	Ref    string `json:"ref"`
}

// InventoryStatus is the owner inventory's status for one kind at evaluation.
type InventoryStatus struct {
	Kind   string `json:"kind"`
	Status string `json:"status"`
}

// Classes of an UnresolvedScope.
const (
	UnresolvedClassFence    = "fence"
	UnresolvedClassConsumer = "consumer"
	UnresolvedClassWriter   = "writer"
)

// Fixed reasons of an UnresolvedScope.
const (
	ReasonFenceMismatch                = "fence-mismatch"
	ReasonIAMReadbackFailed            = "iam-readback-failed"
	ReasonNoAcceptedEnforcement        = "no-accepted-enforcement-contract"
	ReasonNoCompleteOwnerInventory     = "no-complete-owner-inventory"
	ReasonParticipantNotDeclared       = "participant-not-declared"
	ReasonOwnerInventoryIncomplete     = "owner-inventory-incomplete"
	ReasonFinalInventoryRereadNotWired = "final-inventory-reread-not-wired"
)

// ExerciseResult is what a non-finalizing run did after IAM quiescence.
type ExerciseResult struct {
	// Skipped is the fixed reason no cleanup ran; Cleanup is then absent.
	Skipped    string           `json:"skipped,omitempty"`
	Cleanup    *PlatformCleanup `json:"cleanup,omitempty"`
	Accounting []KindAccounting `json:"accounting,omitempty"`
}

// PlatformCleanup is the EC2 platform cleanup result. It carries resource IDs,
// fixed reasons and counts only, never names, tags, key material or data.
type PlatformCleanup struct {
	Kinds  []CleanupKind  `json:"kinds"`
	Items  []CleanupItem  `json:"items,omitempty"`
	Counts []CleanupCount `json:"counts,omitempty"`
}

// CleanupKind is one kind's scan status after the cleanup's own re-scan.
type CleanupKind struct {
	Kind              string   `json:"kind"`
	Status            string   `json:"status"`
	IncompleteReasons []string `json:"incompleteReasons,omitempty"`
}

// CleanupItem is one resource by its ID with what the cleanup did to it.
type CleanupItem struct {
	ResourceID  string `json:"resourceId"`
	Kind        string `json:"kind"`
	Disposition string `json:"disposition"`
	Reason      string `json:"reason,omitempty"`
	MemberCount int    `json:"memberCount,omitempty"`
}

// CleanupCount is the number of records of one class that have no public ID.
type CleanupCount struct {
	Kind        string `json:"kind"`
	Class       string `json:"class"`
	Disposition string `json:"disposition"`
	Reason      string `json:"reason,omitempty"`
	Count       int    `json:"count"`
}

// KindAccounting is the final inventory re-read held against the cleanup result
// for one kind. Quiescence is not rewritten from it.
type KindAccounting struct {
	Kind      string `json:"kind"`
	Status    string `json:"status"`
	Reason    string `json:"reason,omitempty"`
	Remaining int    `json:"remaining"`
}

// Accounting statuses and fixed reasons of a KindAccounting.
const (
	AccountingAccounted   = "accounted"
	AccountingUnaccounted = "unaccounted"

	AccountingReasonRereadIncomplete = "reread-incomplete"
	AccountingReasonUnnamedRemainder = "unnamed-remainder"
)

// Fixed reasons an exercise ran no cleanup.
const (
	ExerciseSkippedNoQuiescedKind = "no-quiesced-kind"
)
