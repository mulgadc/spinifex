package accountlifecyclev1

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestParticipantSubjects(t *testing.T) {
	require.Equal(t, "accountlifecycle.v1.participant.migration.inventory", InventorySubject("migration"))
	require.Equal(t, "accountlifecycle.v1.participant.migration.terminate", TerminateSubject("migration"))
}

func TestValidParticipantName(t *testing.T) {
	for _, ok := range []string{"migration", "sqs", "lambda-2"} {
		require.True(t, ValidParticipantName(ok), ok)
	}
	for _, bad := range []string{"", "a.b", "*", ">", "Migration", "-x", "a b"} {
		require.False(t, ValidParticipantName(bad), bad)
	}
}

func TestInventoryJSONContract(t *testing.T) {
	data, err := json.Marshal(InventoryRequest{AccountID: "111122223333", CorrelationRef: "op-1"})
	require.NoError(t, err)
	require.JSONEq(t, `{"account_id":"111122223333","correlation_ref":"op-1"}`, string(data))

	data, err = json.Marshal(InventoryReply{
		AccountID: "111122223333", Participant: "migration", DeclarationVersion: 1, ContractVersion: 1,
		Classes: []ClassReport{
			{Class: "connection", Remaining: 0, Status: StatusClean},
			{Class: "copy-task", Remaining: 1, Status: StatusIncomplete, Reasons: []string{ReasonStoreError}},
		},
	})
	require.NoError(t, err)
	require.JSONEq(t, `{"account_id":"111122223333","participant":"migration","declaration_version":1,"contract_version":1,
		"classes":[{"class":"connection","remaining":0,"status":"clean"},
		{"class":"copy-task","remaining":1,"status":"incomplete","reasons":["store-error"]}]}`, string(data))
}

func TestTerminateJSONContract(t *testing.T) {
	data, err := json.Marshal(TerminateRequest{AccountID: "111122223333"})
	require.NoError(t, err)
	require.JSONEq(t, `{"account_id":"111122223333"}`, string(data), "the interim engine sends no operation identity or fence")

	data, err = json.Marshal(TerminateRequest{AccountID: "111122223333", OperationID: "op-1", Fence: 7})
	require.NoError(t, err)
	require.JSONEq(t, `{"account_id":"111122223333","operation_id":"op-1","fence":7}`, string(data))

	data, err = json.Marshal(TerminateReply{
		AccountID: "111122223333", Participant: "migration", DeclarationVersion: 1, ContractVersion: 1,
		Classes: []ClassReport{{Class: "assessment", Remaining: 2, Status: StatusPresent}},
		Blocked: []BlockedItem{{Class: "assessment", Reason: ReasonOwnerNotStopped, Count: 2}},
	})
	require.NoError(t, err)
	require.JSONEq(t, `{"account_id":"111122223333","participant":"migration","declaration_version":1,"contract_version":1,
		"classes":[{"class":"assessment","remaining":2,"status":"present"}],
		"blocked":[{"class":"assessment","reason":"owner-not-stopped","count":2}]}`, string(data))
}
