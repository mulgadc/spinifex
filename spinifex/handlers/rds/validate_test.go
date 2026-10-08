package handlers_rds

import (
	"strings"
	"testing"

	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestValidateMasterUserPassword(t *testing.T) {
	t.Parallel()
	assert.NoError(t, ValidateMasterUserPassword("Sup3rSecret!"))
	assert.NoError(t, ValidateMasterUserPassword(strings.Repeat("x", maxMasterPasswordLen)))

	for _, password := range []string{
		"",
		strings.Repeat("x", minMasterPasswordLen-1),
		strings.Repeat("x", maxMasterPasswordLen+1),
		// The characters AWS excludes, which would otherwise break a connection
		// string or the engine's own role syntax.
		"pass/word1", `pass"word1`, "pass@word1", "pass word1",
		// Outside printable ASCII. A newline survives the bootstrap handoff and
		// defeats the guest's line-oriented redaction, putting the password on
		// the host serial console; the rest are refused as the same class.
		"pass\nword1", "pass\rword1", "pass\tword1", "pass\x00word1", "pass\x7fword1",
		"pässword1", "passwordé", "pass word1",
	} {
		err := ValidateMasterUserPassword(password)
		require.Error(t, err, "password %q should be rejected", password)
		assert.Contains(t, err.Error(), awserrors.ErrorInvalidParameterValue)
	}
}
