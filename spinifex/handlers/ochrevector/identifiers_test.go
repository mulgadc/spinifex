package handlers_ochrevector

import (
	"testing"

	"github.com/mulgadc/spinifex/spinifex/utils"
	"github.com/stretchr/testify/assert"
)

func TestValidateAccountID(t *testing.T) {
	assert.NoError(t, validateAccountID(utils.GlobalAccountID))
	assert.NoError(t, validateAccountID("123456789012"))

	assert.Error(t, validateAccountID(""))
	assert.Error(t, validateAccountID("not-an-account"))
	assert.Error(t, validateAccountID("12345"))
	assert.Error(t, validateAccountID("1234567890123"))
}

func TestValidateIndexID(t *testing.T) {
	assert.NoError(t, validateIndexID("idx-0123456789abcdef1"))
	assert.NoError(t, validateIndexID("a"))
	assert.NoError(t, validateIndexID("my_index-1"))

	assert.Error(t, validateIndexID(""))
	assert.Error(t, validateIndexID("-leading-dash"))
	assert.Error(t, validateIndexID("has a space"))
	assert.Error(t, validateIndexID("has;semicolon"))
	assert.Error(t, validateIndexID("has\"quote"))
}

func TestSchemaAndRoleNames(t *testing.T) {
	assert.Equal(t, "kb_"+utils.GlobalAccountID, schemaName(utils.GlobalAccountID))
	assert.Equal(t, "kb_"+utils.GlobalAccountID+"_role", roleName(utils.GlobalAccountID))
}

func TestTableAndIndexNames(t *testing.T) {
	assert.Equal(t, "idx_myindex", tableName("myindex"))
	assert.Equal(t, "idx_myindex_embedding_hnsw", hnswIndexName("myindex"))
}

func TestSanitizeIdent(t *testing.T) {
	assert.Equal(t, `"kb_000000000000"`, sanitizeIdent("kb_000000000000"))
	// A name containing a double quote is escaped by doubling it, pgx's own
	// defense against an identifier that reaches Sanitize despite the
	// allowlist regex rejecting it earlier.
	assert.Equal(t, `"weird""name"`, sanitizeIdent(`weird"name`))
}
