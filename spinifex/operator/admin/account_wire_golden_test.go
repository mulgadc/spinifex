package admin

import (
	"encoding/json"
	"testing"

	operatorv1 "github.com/mulgadc/spinifex/contracts/operator/v1"
	"github.com/mulgadc/spinifex/spinifex/domains/admission/quota"
	accountteardown "github.com/mulgadc/spinifex/spinifex/operator/accountlifecycle"
	"github.com/stretchr/testify/require"
)

func goldenJSON(t *testing.T, v any, want string) {
	t.Helper()
	data, err := json.Marshal(v)
	require.NoError(t, err)
	require.JSONEq(t, want, string(data))
}

func intPtr(v int) *int { return &v }

func TestAdminWire_DeleteAccountResponse(t *testing.T) {
	goldenJSON(t, DeleteAccountResponse{DeletionID: "del-1", AccountID: "000000000042", State: operatorv1.DeletionStateRunning},
		`{"deletionId":"del-1","accountId":"000000000042","state":"RUNNING","dryRun":false}`)

	inventory := &accountteardown.Result{
		AccountID: "000000000042", AccountName: "acme", DryRun: true,
		Stages: []accountteardown.StageResult{{
			Stage:   accountteardown.StageCompute,
			Deleted: []accountteardown.Resource{{Kind: "instance", ID: "i-1", Detail: "node-1"}},
			Stuck:   []accountteardown.Stuck{{Resource: accountteardown.Resource{Kind: "volume", ID: "vol-1"}, Reason: "attached"}},
			Elapsed: "1s",
		}},
	}
	data, err := json.Marshal(DeleteAccountResponse{DeletionID: "del-2", AccountID: "000000000042", State: operatorv1.DeletionStateCompleted, DryRun: true, Inventory: inventory})
	require.NoError(t, err)
	var got map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(data, &got))
	require.JSONEq(t, `"del-2"`, string(got["deletionId"]))
	require.JSONEq(t, `"COMPLETED"`, string(got["state"]))
	require.JSONEq(t, `true`, string(got["dryRun"]))
	require.JSONEq(t, `{
		"accountId":"000000000042","accountName":"acme","dryRun":true,"forced":false,"accountDeleted":false,
		"stages":[{"stage":1,"deleted":[{"kind":"instance","id":"i-1","detail":"node-1"}],
			"stuck":[{"resource":{"kind":"volume","id":"vol-1"},"reason":"attached"}],"elapsed":"1s"}]
	}`, string(got["inventory"]))
}

func TestAdminWire_AccountQuotaRequest(t *testing.T) {
	goldenJSON(t, AccountQuotaRequest{AccountID: "000000000042"}, `{"accountId":"000000000042"}`)
	goldenJSON(t, AccountQuotaRequest{AccountID: "000000000042", Overrides: quota.Overrides{VCPUs: intPtr(0), VPCs: intPtr(5)}},
		`{"accountId":"000000000042","overrides":{"vcpus":0,"vpcs":5}}`)
}

func TestAdminWire_AccountQuotaResponse(t *testing.T) {
	goldenJSON(t, AccountQuotaResponse{
		AccountID: "000000000042",
		Limits:    map[string]int{"vcpus": 8},
		Source:    map[string]string{"vcpus": "override"},
		Overrides: quota.Overrides{VCPUs: intPtr(8), UpdatedBy: "ops"},
	}, `{"accountId":"000000000042","limits":{"vcpus":8},"source":{"vcpus":"override"},"overrides":{"vcpus":8,"updated_by":"ops"}}`)
}
