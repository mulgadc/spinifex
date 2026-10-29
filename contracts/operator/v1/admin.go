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
