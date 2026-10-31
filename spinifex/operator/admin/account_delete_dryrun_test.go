package admin

//test:in-package — the deletion handler and its request-ID context are
// unexported.

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"strings"
	"sync"
	"testing"

	accountteardown "github.com/mulgadc/spinifex/spinifex/operator/accountlifecycle"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const dryRunShadowMessage = "Account teardown dry-run EC2 platform shadow comparison"

// syncBuffer is a log sink safe for the gateway's concurrent writers.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func captureDefaultLog(t *testing.T) *syncBuffer {
	t.Helper()
	logs := &syncBuffer{}
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(logs, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return logs
}

// The admin API dry run answers exactly as the plain inventory would, and its
// shadow comparison is logged under the request ID the response carries.
func TestAdminDeleteAccountDryRunLogsTheShadowUnderTheRequestID(t *testing.T) {
	gw := newDeleteAccountGateway(t)
	account := newTenantAccount(t, gw, "tenant@example.com")
	logs := captureDefaultLog(t)

	const requestID = "req-dryrun-1"
	got, err := gw.deleteAccount(WithRequestID(t.Context(), requestID),
		[]byte(`{"accountId":"`+account.AccountID+`","dryRun":true}`))
	require.NoError(t, err)
	gotJSON, err := json.Marshal(got)
	require.NoError(t, err)

	engine, err := accountteardown.NewClusterEngine(t.Context(), gw.NATS, gw.ExpectedNodes, gw.NodeIDs, gw.IAM, gw.BucketStore)
	require.NoError(t, err)
	inventory, err := engine.Inventory(t.Context(), account.AccountID)
	require.NoError(t, err)
	want, err := json.Marshal(&DeleteAccountResponse{AccountID: account.AccountID, State: "DRY_RUN", DryRun: true, Inventory: inventory})
	require.NoError(t, err)
	assert.JSONEq(t, string(want), string(gotJSON))

	var records []map[string]any
	for line := range strings.SplitSeq(strings.TrimSpace(logs.String()), "\n") {
		var record map[string]any
		if json.Unmarshal([]byte(line), &record) == nil && record["msg"] == dryRunShadowMessage {
			records = append(records, record)
		}
	}
	require.Len(t, records, 1)
	assert.Equal(t, requestID, records[0]["correlationRef"])
	assert.Equal(t, account.AccountID, records[0]["accountID"])
	// The stub cluster answers every ec2 subject with an empty body, which is
	// no inventory for this account, so the comparison cannot be clean.
	assert.Equal(t, "scan-incomplete", records[0]["outcome"])
}
