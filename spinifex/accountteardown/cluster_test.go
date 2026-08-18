package accountteardown

//test:in-package — the reaper types the wiring registers are unexported, and
// what this asserts is that both surfaces get the full set of them.

import (
	"testing"

	handlers_iam "github.com/mulgadc/spinifex/spinifex/handlers/iam"
	"github.com/mulgadc/spinifex/spinifex/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The CLI and the admin API share this wiring precisely so neither can be
// missing a reaper. A stage with none registered would silently skip whatever
// it owns, leaving resources behind that nothing else deletes.
func TestNewClusterEngineCoversEveryStage(t *testing.T) {
	_, nc, _ := testutil.StartTestJetStream(t)

	masterKey, err := handlers_iam.GenerateMasterKey()
	require.NoError(t, err)
	svc, err := handlers_iam.NewIAMServiceImpl(t.Context(), nc, masterKey, 1)
	require.NoError(t, err)

	engine, err := NewClusterEngine(t.Context(), nc, 1, svc, newFakeBuckets())
	require.NoError(t, err)
	require.NotNil(t, engine.Accounts)

	// A teardown with no object store would report every stage drained and
	// still delete the account record, leaving the tenant's objects behind.
	_, err = NewClusterEngine(t.Context(), nc, 1, svc, nil)
	require.Error(t, err)

	for _, stage := range Stages() {
		if stage == StageAttachments {
			// Attachments has no reaper of its own: it re-checks what stage 1 left.
			continue
		}
		assert.NotEmpty(t, engine.reapersFor(stage), "stage %s has no reaper", stage.String())
	}
}

// A teardown must refuse the accounts no credential may delete before it reads
// anything, and refuse a name confirmation that does not match the record.
func TestClusterEnginePrechecks(t *testing.T) {
	_, nc, _ := testutil.StartTestJetStream(t)

	masterKey, err := handlers_iam.GenerateMasterKey()
	require.NoError(t, err)
	svc, err := handlers_iam.NewIAMServiceImpl(t.Context(), nc, masterKey, 1)
	require.NoError(t, err)

	engine, err := NewClusterEngine(t.Context(), nc, 1, svc, newFakeBuckets())
	require.NoError(t, err)

	// Account ids are sequential and the first is the protected super admin.
	_, err = svc.CreateAccount("super-admin@example.com")
	require.NoError(t, err)
	tenant, err := svc.CreateAccount("tenant@example.com")
	require.NoError(t, err)

	assert.ErrorIs(t, engine.Precheck("000000000000", ""), ErrProtectedAccount)
	assert.ErrorIs(t, engine.Precheck("000000000001", ""), ErrProtectedAccount)
	assert.ErrorIs(t, engine.Precheck(tenant.AccountID, "wrong@example.com"), ErrAccountNameMismatch)
	assert.NoError(t, engine.Precheck(tenant.AccountID, "tenant@example.com"))
	assert.Error(t, engine.Precheck("000000009999", ""))
}
