package operatorv1

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

func goldenJSON(t *testing.T, v any, want string) {
	t.Helper()
	data, err := json.Marshal(v)
	require.NoError(t, err)
	require.JSONEq(t, want, string(data))
}

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

func TestAdminWire_DescribeAccountDeletionRequest(t *testing.T) {
	goldenJSON(t, DescribeAccountDeletionRequest{AccountID: "000000000042"}, `{"accountId":"000000000042"}`)
}

func TestAdminWire_DeletionStates(t *testing.T) {
	require.Equal(t, "RUNNING", DeletionStateRunning)
	require.Equal(t, "COMPLETED", DeletionStateCompleted)
	require.Equal(t, "FAILED", DeletionStateFailed)
	require.Equal(t, "EXERCISED", DeletionStateExercised)
}

func TestAdminWire_NonFinalizingDeleteRequest(t *testing.T) {
	goldenJSON(t, DeleteAccountRequest{AccountID: "000000000042", AccountName: "acme", ClientToken: "tok-3", NonFinalizing: true},
		`{"accountId":"000000000042","accountName":"acme","clientToken":"tok-3","nonFinalizing":true}`)
}

func TestAdminWire_ExerciseResult(t *testing.T) {
	goldenJSON(t, ExerciseResult{
		Cleanup: &PlatformCleanup{
			Kinds:  []CleanupKind{{Kind: "key-pair", Status: "observed-clean-at-scan-time"}},
			Items:  []CleanupItem{{ResourceID: "pg-1", Kind: "placement-group", Disposition: "blocked", Reason: "instance-members", MemberCount: 1}},
			Counts: []CleanupCount{{Kind: "key-pair", Class: "public-key-object-without-metadata", Disposition: "deleted", Count: 2}},
		},
		Accounting: []KindAccounting{{Kind: "placement-group", Status: AccountingAccounted, Remaining: 1}},
	}, `{"cleanup":{"kinds":[{"kind":"key-pair","status":"observed-clean-at-scan-time"}],
		"items":[{"resourceId":"pg-1","kind":"placement-group","disposition":"blocked","reason":"instance-members","memberCount":1}],
		"counts":[{"kind":"key-pair","class":"public-key-object-without-metadata","disposition":"deleted","count":2}]},
		"accounting":[{"kind":"placement-group","status":"accounted","remaining":1}]}`)
	goldenJSON(t, ExerciseResult{Skipped: ExerciseSkippedNoQuiescedKind}, `{"skipped":"no-quiesced-kind"}`)
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
