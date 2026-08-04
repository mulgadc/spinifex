package handlers_rds

import (
	"strings"
	"testing"

	"github.com/mulgadc/spinifex/spinifex/awserrors"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLookupEngine(t *testing.T) {
	_, err := LookupEngine("PostgreSQL")
	require.Error(t, err, "the AWS engine identifier is 'postgres', not the product name")

	engine, err := LookupEngine("  POSTGRES ")
	require.NoError(t, err, "the engine name is matched case-insensitively and trimmed")
	assert.Equal(t, "postgres", engine.Name)
	assert.Equal(t, int64(5432), engine.DefaultPort)
	assert.Equal(t, "default.postgres18", engine.DefaultParameterGroupName())

	_, err = LookupEngine("mysql")
	require.Error(t, err)
	assert.Contains(t, err.Error(), awserrors.ErrorInvalidParameterValue)
	assert.Equal(t, []string{"postgres"}, SupportedEngines())
}

// A version other than the pinned one would be served by an image that is
// not the one asked for, so it is rejected rather than quietly substituted.
func TestEngineValidateVersion(t *testing.T) {
	engine, err := LookupEngine("postgres")
	require.NoError(t, err)

	for _, version := range []string{"", "18", "18.1", "18.4"} {
		assert.NoError(t, engine.ValidateVersion(version), "version %q", version)
	}
	for _, version := range []string{"17", "16.2", "19", "latest"} {
		assert.Error(t, engine.ValidateVersion(version), "version %q", version)
	}

	// The pin, not the request, is what the record and the AMI lookup carry.
	assert.Equal(t, "18", engine.EngineVersion())
}

func TestEngineValidateMasterUsername(t *testing.T) {
	engine, err := LookupEngine("postgres")
	require.NoError(t, err)

	for _, username := range []string{"appuser", "app_user1", "Orders"} {
		assert.NoError(t, engine.ValidateMasterUsername(username), "username %q", username)
	}

	cases := map[string]string{
		"empty":          "",
		"leading digit":  "1user",
		"hyphen":         "app-user",
		"too long":       strings.Repeat("a", maxMasterUsernameLen+1),
		"reserved":       "rdsadmin",
		"reserved cased": "RDSAdmin",
		// The cluster superuser and the group role the master's administrative
		// privileges come through. Both exist by the time the master is created,
		// so a collision would surface as a failed bootstrap inside the guest.
		"cluster superuser": "postgres",
		"group role":        "rds_superuser",
		// postgres reserves the pg_ prefix for its own internal roles, so a master
		// role taking one would collide inside the engine, not at the API.
		"reserved prefix": "pg_backup",
	}
	for name, username := range cases {
		t.Run(name, func(t *testing.T) {
			err := engine.ValidateMasterUsername(username)
			require.Error(t, err)
			assert.Contains(t, err.Error(), awserrors.ErrorInvalidParameterValue)
		})
	}
}

func TestValidateMasterUserPassword(t *testing.T) {
	assert.NoError(t, ValidateMasterUserPassword("Sup3rSecret!"))
	assert.NoError(t, ValidateMasterUserPassword(strings.Repeat("x", maxMasterPasswordLen)))

	for _, password := range []string{
		"",
		strings.Repeat("x", minMasterPasswordLen-1),
		strings.Repeat("x", maxMasterPasswordLen+1),
		// The characters AWS excludes, which would otherwise break a connection
		// string or the engine's own role syntax.
		"pass/word1", `pass"word1`, "pass@word1", "pass word1",
	} {
		err := ValidateMasterUserPassword(password)
		require.Error(t, err, "password %q should be rejected", password)
		assert.Contains(t, err.Error(), awserrors.ErrorInvalidParameterValue)
	}
}
