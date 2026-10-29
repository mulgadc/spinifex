package gateway

import (
	"encoding/json"
	"testing"

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

func TestAdminWire_CreateAccountRequest(t *testing.T) {
	goldenJSON(t, CreateAccountRequest{Name: "acme", ClientToken: "tok-1", Source: "cli"},
		`{"name":"acme","clientToken":"tok-1","source":"cli"}`)
	goldenJSON(t, CreateAccountRequest{Name: "acme", ClientToken: "tok-1"},
		`{"name":"acme","clientToken":"tok-1"}`)
}

func TestAdminWire_CreateAccountResponse(t *testing.T) {
	goldenJSON(t, CreateAccountResponse{
		AccountID: "000000000042", AccountName: "acme", AdminUser: "admin",
		AccessKeyID: "AKIAEXAMPLE", SecretAccessKey: "secret", DefaultVpcID: "vpc-1",
	}, `{"accountId":"000000000042","accountName":"acme","adminUser":"admin","accessKeyId":"AKIAEXAMPLE","secretAccessKey":"secret","defaultVpcId":"vpc-1"}`)
}

func TestAdminWire_DeleteAccountRequest(t *testing.T) {
	goldenJSON(t, DeleteAccountRequest{AccountID: "000000000042", AccountName: "acme", ClientToken: "tok-2", Force: true, DryRun: true},
		`{"accountId":"000000000042","accountName":"acme","clientToken":"tok-2","force":true,"dryRun":true}`)
	goldenJSON(t, DeleteAccountRequest{AccountID: "000000000042", AccountName: "acme", ClientToken: "tok-2"},
		`{"accountId":"000000000042","accountName":"acme","clientToken":"tok-2"}`)
}

func TestAdminWire_DeleteAccountResponse(t *testing.T) {
	goldenJSON(t, DeleteAccountResponse{DeletionID: "del-1", AccountID: "000000000042", State: DeletionStateRunning},
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
	data, err := json.Marshal(DeleteAccountResponse{DeletionID: "del-2", AccountID: "000000000042", State: DeletionStateCompleted, DryRun: true, Inventory: inventory})
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

func TestAdminWire_DescribeAccountDeletionRequest(t *testing.T) {
	goldenJSON(t, DescribeAccountDeletionRequest{AccountID: "000000000042"}, `{"accountId":"000000000042"}`)
}

func TestAdminWire_DeletionStates(t *testing.T) {
	require.Equal(t, "RUNNING", DeletionStateRunning)
	require.Equal(t, "COMPLETED", DeletionStateCompleted)
	require.Equal(t, "FAILED", DeletionStateFailed)
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

func TestAdminWire_ListAccountsResponse(t *testing.T) {
	goldenJSON(t, ListAccountsResponse{
		Accounts: []AccountSummary{
			{AccountID: "000000000042", AccountName: "acme", Status: "ACTIVE", CreatedAt: "2026-01-02T03:04:05Z"},
			{AccountID: "000000000043", AccountName: "bolt", Status: "TERMINATING"},
		},
		Count: 2,
	}, `{"accounts":[
		{"accountId":"000000000042","accountName":"acme","status":"ACTIVE","createdAt":"2026-01-02T03:04:05Z"},
		{"accountId":"000000000043","accountName":"bolt","status":"TERMINATING"}],"count":2}`)
}

func TestAdminWire_AdminMethodNames(t *testing.T) {
	require.Equal(t, []string{
		"CreateAccount", "DeleteAccount", "DescribeAccountDeletion",
		"GetAccountQuota", "ListAccounts", "PutAccountQuota",
	}, AdminMethodNames())
}
