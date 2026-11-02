package accountlifecycle

//test:in-package — the reaper types the wiring registers are unexported, and
// what this asserts is that both surfaces get the full set of them.

import (
	"testing"

	"github.com/mulgadc/spinifex/internal/testkit"
	iamdomain "github.com/mulgadc/spinifex/spinifex/domains/iam"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func testClusterNodes() ClusterNodes {
	return ClusterNodes{Expected: 1, DaemonIDs: []string{"node-1"}, GatewayIDs: []string{"node-1"}}
}

// The CLI and the admin API share this wiring precisely so neither can be
// missing a reaper. A stage with none registered would silently skip whatever
// it owns, leaving resources behind that nothing else deletes.
func TestNewClusterEngineCoversEveryStage(t *testing.T) {
	_, nc, _ := testutil.StartTestJetStream(t)

	masterKey, err := iamdomain.GenerateMasterKey()
	require.NoError(t, err)
	svc, err := iamdomain.NewIAMServiceImpl(t.Context(), nc, masterKey)
	require.NoError(t, err)

	engine, err := NewClusterEngine(t.Context(), nc, testClusterNodes(), svc, newFakeBuckets())
	require.NoError(t, err)
	require.NotNil(t, engine.Accounts)

	// A teardown with no object store would report every stage drained and
	// still delete the account record, leaving the tenant's objects behind.
	_, err = NewClusterEngine(t.Context(), nc, testClusterNodes(), svc, nil)
	require.Error(t, err)

	for _, stage := range Stages() {
		if stage == StageAttachments {
			// Attachments has no reaper of its own: it re-checks what stage 1 left.
			continue
		}
		assert.NotEmpty(t, engine.reapersFor(stage), "stage %s has no reaper", stage.String())
	}
}

// The EC2 settings seal reaper must be registered in StageAccount on every
// surface that builds a cluster engine, or teardown would finalise accounts
// without ever sealing their settings.
func TestNewClusterEngineRegistersTheEC2SettingsSealReaper(t *testing.T) {
	_, nc, _ := testutil.StartTestJetStream(t)

	masterKey, err := iamdomain.GenerateMasterKey()
	require.NoError(t, err)
	svc, err := iamdomain.NewIAMServiceImpl(t.Context(), nc, masterKey)
	require.NoError(t, err)

	engine, err := NewClusterEngine(t.Context(), nc, testClusterNodes(), svc, newFakeBuckets())
	require.NoError(t, err)

	var kinds []string
	for _, reaper := range engine.reapersFor(StageAccount) {
		kinds = append(kinds, reaper.Kind())
	}
	assert.Contains(t, kinds, "ec2-account-settings")
}

// The quota seal reaper must be registered in StageAccount on every surface,
// and must probe the gateway nodes, not the daemon nodes: a swapped set would
// compile and probe processes that never write an override.
func TestNewClusterEngineRegistersTheQuotaOverrideSealReaper(t *testing.T) {
	_, nc, _ := testutil.StartTestJetStream(t)

	masterKey, err := iamdomain.GenerateMasterKey()
	require.NoError(t, err)
	svc, err := iamdomain.NewIAMServiceImpl(t.Context(), nc, masterKey)
	require.NoError(t, err)

	nodes := ClusterNodes{Expected: 3, DaemonIDs: []string{"d-1", "d-2", "d-3"}, GatewayIDs: []string{"gw-1"}}
	engine, err := NewClusterEngine(t.Context(), nc, nodes, svc, newFakeBuckets())
	require.NoError(t, err)

	var quotaReaper *quotaOverrideSealReaper
	var ec2Reaper *accountSettingsSealReaper
	for _, reaper := range engine.reapersFor(StageAccount) {
		switch r := reaper.(type) {
		case *quotaOverrideSealReaper:
			quotaReaper = r
		case *accountSettingsSealReaper:
			ec2Reaper = r
		}
	}
	require.NotNil(t, quotaReaper, "quota-override reaper not registered in StageAccount")
	require.NotNil(t, ec2Reaper)
	assert.Equal(t, []string{"gw-1"}, quotaReaper.capability.(*sealCapabilityClient).nodeIDs)
	assert.Equal(t, []string{"d-1", "d-2", "d-3"}, ec2Reaper.capability.(*sealCapabilityClient).nodeIDs)
}

// A teardown must refuse the accounts no credential may delete before it reads
// anything, and refuse a name confirmation that does not match the record.
func TestClusterEnginePrechecks(t *testing.T) {
	_, nc, _ := testutil.StartTestJetStream(t)

	masterKey, err := iamdomain.GenerateMasterKey()
	require.NoError(t, err)
	svc, err := iamdomain.NewIAMServiceImpl(t.Context(), nc, masterKey)
	require.NoError(t, err)

	engine, err := NewClusterEngine(t.Context(), nc, testClusterNodes(), svc, newFakeBuckets())
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
