package admin

//test:in-package — the deletion handler is unexported.

import (
	"encoding/json"
	"testing"

	accountteardown "github.com/mulgadc/spinifex/spinifex/operator/accountlifecycle"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The admin API dry run answers exactly as the plain inventory would, runs no
// cleanup and leaves no job record.
func TestAdminDeleteAccountDryRunAnswersTheInventoryAndCleansNothing(t *testing.T) {
	t.Parallel()
	gw := newDeleteAccountGateway(t)
	cleaner := &panicCleaner{}
	gw.PlatformCleaner = cleaner
	account := newTenantAccount(t, gw, "tenant@example.com")

	got, err := gw.deleteAccount(WithRequestID(t.Context(), "req-dryrun-1"),
		[]byte(`{"accountId":"`+account.AccountID+`","dryRun":true}`))
	require.NoError(t, err)
	gotJSON, err := json.Marshal(got)
	require.NoError(t, err)

	engine, err := accountteardown.NewClusterEngine(t.Context(), gw.NATS, accountteardown.ClusterNodes{Expected: gw.ExpectedNodes, DaemonIDs: gw.NodeIDs, GatewayIDs: gw.GatewayNodeIDs}, gw.Termination, gw.BucketStore, gw.PlatformCleaner, accountteardown.WithComputeCleanup(gw.ComputeCleaner, gw.StorageCleaner))
	require.NoError(t, err)
	inventory, err := engine.Inventory(t.Context(), account.AccountID)
	require.NoError(t, err)
	want, err := json.Marshal(&DeleteAccountResponse{AccountID: account.AccountID, State: "DRY_RUN", DryRun: true, Inventory: inventory})
	require.NoError(t, err)
	assert.JSONEq(t, string(want), string(gotJSON))
	assert.Zero(t, cleaner.calls)
}
