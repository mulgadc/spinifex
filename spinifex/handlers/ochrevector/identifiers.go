package handlers_ochrevector

import (
	"fmt"
	"regexp"

	"github.com/jackc/pgx/v5"
	"github.com/mulgadc/spinifex/spinifex/utils"
)

// indexIDPattern is a conservative allowlist for anything that reaches a SQL
// identifier: utils.GenerateResourceID's own alphabet ("idx-<17 hex>") plus
// enough headroom that a caller-supplied id still round-trips safely rather
// than being silently rejected.
var indexIDPattern = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_-]{0,62}$`)

// validateAccountID rejects anything that is not a well-formed 12-digit
// AWS-style account id before it ever reaches a SQL identifier or
// search_path.
func validateAccountID(accountID string) error {
	if !utils.IsAccountID(accountID) {
		return fmt.Errorf("ochrevector: invalid account id %q", accountID)
	}
	return nil
}

// validateIndexID rejects anything outside a conservative identifier
// allowlist before it ever reaches a SQL identifier.
func validateIndexID(indexID string) error {
	if !indexIDPattern.MatchString(indexID) {
		return fmt.Errorf("ochrevector: invalid index id %q", indexID)
	}
	return nil
}

// schemaName returns accountID's schema name. Callers must validateAccountID
// first.
func schemaName(accountID string) string {
	return "kb_" + accountID
}

// roleName returns accountID's non-login role name. Callers must
// validateAccountID first.
func roleName(accountID string) string {
	return "kb_" + accountID + "_role"
}

// tableName returns indexID's backing table name. Callers must
// validateIndexID first.
func tableName(indexID string) string {
	return "idx_" + indexID
}

// hnswIndexName returns indexID's HNSW index name. Callers must
// validateIndexID first.
func hnswIndexName(indexID string) string {
	return "idx_" + indexID + "_embedding_hnsw"
}

// sanitizeIdent quotes name as a single SQL identifier via pgx's own
// sanitizer — the last line of defense against injection even though every
// caller has already passed name through an allowlist regex first.
func sanitizeIdent(name string) string {
	return pgx.Identifier{name}.Sanitize()
}
