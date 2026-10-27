package gateway

//test:in-package — the admin dispatcher's request-ID context and the
// deletion handler it reaches are unexported.

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"testing"

	iamdomain "github.com/mulgadc/spinifex/spinifex/domains/iam"
	accountteardown "github.com/mulgadc/spinifex/spinifex/operator/accountlifecycle"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const dryRunShadowMessage = "Account teardown dry-run EC2 platform shadow comparison"

// deleteAccountGrant is the real IAM service with a policy granting
// spinifex:DeleteAccount, so a request clears the admin gates.
type deleteAccountGrant struct {
	iamdomain.IAMService
}

func (deleteAccountGrant) GetUserPolicies(string, string) ([]iamdomain.PolicyDocument, error) {
	return []iamdomain.PolicyDocument{{
		Version: "2012-10-17",
		Statement: []iamdomain.Statement{{
			Effect: "Allow", Action: iamdomain.StringOrArr{"spinifex:DeleteAccount"}, Resource: iamdomain.StringOrArr{"*"},
		}},
	}}, nil
}

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
	gw.IAMService = deleteAccountGrant{IAMService: gw.IAMService}
	account := newTenantAccount(t, gw, "tenant@example.com")
	logs := captureDefaultLog(t)

	rec := authorizedAdminRequestForMethod(gw, "DeleteAccount", `{"accountId":"`+account.AccountID+`","dryRun":true}`)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	requestID := rec.Header().Get("X-Amzn-Requestid")
	require.NotEmpty(t, requestID)

	engine, err := accountteardown.NewClusterEngine(t.Context(), gw.NATSConn, gw.ExpectedNodes, gw.NodeIDs, gw.IAMService, gw.BucketStore)
	require.NoError(t, err)
	inventory, err := engine.Inventory(t.Context(), account.AccountID)
	require.NoError(t, err)
	want, err := json.Marshal(&DeleteAccountResponse{AccountID: account.AccountID, State: "DRY_RUN", DryRun: true, Inventory: inventory})
	require.NoError(t, err)
	assert.JSONEq(t, string(want), rec.Body.String())

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
