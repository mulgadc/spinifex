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
// fence. Recording only: it admits nothing, and QuiescedScope stays empty until
// a final inventory re-read exists to catch a late write.
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
