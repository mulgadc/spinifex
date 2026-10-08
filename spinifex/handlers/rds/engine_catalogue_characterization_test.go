package handlers_rds

//test:in-package — pins present behaviour of the engine-catalogue consumers
// that stay in this package after the split: the recovery-message text built
// from the engine's crash/unclean-stop notes, and this package's own
// ValidateMasterUserPassword.

import (
	"testing"

	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ValidateMasterUserPassword's three rejections all go through awserrors.Errorf,
// so each one resolves through ValidErrorCodeFromError to the same code.
func TestValidateMasterUserPassword_ExactCodesAndMessages(t *testing.T) {
	t.Parallel()

	empty := ValidateMasterUserPassword("")
	require.Error(t, empty)
	assert.Equal(t, "MasterUserPassword is required: InvalidParameterValue", empty.Error())
	assert.Equal(t, awserrors.ErrorInvalidParameterValue, awserrors.ValidErrorCodeFromError(empty))

	tooShort := ValidateMasterUserPassword("short1")
	require.Error(t, tooShort)
	assert.Equal(t, "MasterUserPassword must be between 8 and 128 characters: InvalidParameterValue", tooShort.Error())
	assert.Equal(t, awserrors.ErrorInvalidParameterValue, awserrors.ValidErrorCodeFromError(tooShort),
		"this one is built with awserrors.Errorf, so it resolves correctly")

	tooLong := ValidateMasterUserPassword(repeatByte('x', maxMasterPasswordLen+1))
	require.Error(t, tooLong)
	assert.Equal(t, "MasterUserPassword must be between 8 and 128 characters: InvalidParameterValue", tooLong.Error())
	assert.Equal(t, awserrors.ErrorInvalidParameterValue, awserrors.ValidErrorCodeFromError(tooLong))
}

func repeatByte(b byte, n int) string {
	out := make([]byte, n)
	for i := range out {
		out[i] = b
	}
	return string(out)
}

// The half of each recovery warning that depends on the engine, pinned
// verbatim rather than by substring: a wording change to either note should
// fail here even if it keeps every substring the existing Contains assertions
// check for.
func TestUncleanStopMessage_PinsExactTextPerEngine(t *testing.T) {
	t.Parallel()
	postgres := uncleanStopMessage(t.Context(), "postgres", "stopping the instance")
	assert.Equal(t,
		"The database engine could not be shut down cleanly before stopping the instance. "+
			"It will recover from its write-ahead log on the next start.",
		postgres)

	mariadb := uncleanStopMessage(t.Context(), "mariadb", "rebooting the instance")
	assert.Equal(t,
		"The database engine could not be shut down cleanly before rebooting the instance. "+
			"InnoDB tables will recover from the redo log on the next start; "+
			"non-transactional tables such as Aria and MyISAM may be left inconsistent.",
		mariadb)
}

func TestCrashConsistentSnapshotMessage_PinsExactTextPerEngine(t *testing.T) {
	t.Parallel()
	postgres := crashConsistentSnapshotMessage(t.Context(), "postgres")
	assert.Equal(t,
		"The database engine could not be quiesced before the snapshot; the snapshot is crash consistent. "+
			"It will recover from its write-ahead log when it is restored.",
		postgres)

	mariadb := crashConsistentSnapshotMessage(t.Context(), "mariadb")
	assert.Equal(t,
		"The database engine could not be quiesced before the snapshot; the snapshot is crash consistent. "+
			"InnoDB tables will recover from the redo log when it is restored; "+
			"non-transactional tables such as Aria and MyISAM may be left inconsistent.",
		mariadb)
}
