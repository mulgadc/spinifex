package admin

//test:in-package — the job record, the tracker and the takeover that clears the
// evaluation are unexported.

import (
	"encoding/json"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	operatorv1 "github.com/mulgadc/spinifex/contracts/operator/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func sampleQuiescence(fence uint64) operatorv1.IAMQuiescence {
	return operatorv1.IAMQuiescence{
		OperationID: "op-1", Fence: fence, Generation: 4,
		QuiescedScope: []string{},
		Unresolved: []operatorv1.UnresolvedScope{{
			Scope: "ec2-key-pair", Class: operatorv1.UnresolvedClassWriter,
			Reason: operatorv1.ReasonFinalInventoryRereadNotWired,
		}},
		Consumers:   []operatorv1.ConsumerBasis{{Consumer: "awsgw-admission", Basis: "ADR-0005:S7"}},
		Writers:     []operatorv1.AccountedWriter{{Writer: "daemon-nats-handlers", Basis: "owner-inventory", Ref: "mulga-tpoz4"}},
		Inventory:   []operatorv1.InventoryStatus{{Kind: "key-pair", Status: "observed-clean-at-scan-time"}},
		EvaluatedAt: "2026-11-02T00:00:00Z",
	}
}

// A normal deletion leaves the evaluation on the record, read back from the real
// IAM account record, and DescribeAccountDeletion returns it.
func TestDeletionRecordCarriesTheIAMQuiescence(t *testing.T) {
	gw := newDeleteAccountGateway(t)
	account := newTenantAccount(t, gw, "tenant@example.com")

	_, err := gw.deleteAccount(t.Context(),
		deleteAccountBody(t, account.AccountID, "tenant@example.com", deleteClientToken))
	require.NoError(t, err)
	require.Eventually(t, func() bool {
		return describeDeletion(t, gw, account.AccountID).State == operatorv1.DeletionStateCompleted
	}, 30*time.Second, 100*time.Millisecond)

	stored := describeDeletion(t, gw, account.AccountID)
	require.NotNil(t, stored.IAM)
	assert.Equal(t, stored.OperationID, stored.IAM.OperationID)
	assert.Equal(t, uint64(1), stored.IAM.Fence)
	assert.Positive(t, stored.IAM.Generation)
	assert.Empty(t, stored.IAM.QuiescedScope)
	assert.NotEmpty(t, stored.IAM.Unresolved)
	assert.Len(t, stored.IAM.Consumers, 3)
	// The stubbed cluster answers the inventory subject with an empty body, which
	// is an incomplete inventory, and recording that must not fail the teardown.
	for _, kind := range []string{"ec2-key-pair", "ec2-placement-group", "ec2-launch-template"} {
		var reasons []string
		for _, entry := range stored.IAM.Unresolved {
			if entry.Scope == kind {
				reasons = append(reasons, entry.Reason)
			}
		}
		assert.Equal(t, []string{operatorv1.ReasonOwnerInventoryIncomplete}, reasons, kind)
	}
}

// The evaluation belongs to a fence: a holder that has been taken over cannot
// write it into the new holder's record.
func TestSupersededHolderCannotWriteTheQuiescence(t *testing.T) {
	gw := newDeleteAccountGateway(t)
	jobs, err := gw.accountDeletionStore(t.Context())
	require.NoError(t, err)
	const accountID = "000000000042"

	holderA, revisionA, err := claimAs(t, jobs, accountID, strings.Repeat("a", 32), "node-a")
	require.NoError(t, err)
	var cancelled atomic.Bool
	trackerA := &deletionTracker{jobs: jobs, job: holderA, revision: revisionA, cancel: func() { cancelled.Store(true) }}

	expireLease(t, jobs, accountID)
	_, _, err = claimAs(t, jobs, accountID, strings.Repeat("b", 32), "node-b")
	require.NoError(t, err)

	trackerA.quiesced(t.Context(), sampleQuiescence(1))

	assert.True(t, cancelled.Load())
	stored := describeDeletion(t, gw, accountID)
	assert.Equal(t, uint64(2), stored.Fence)
	assert.Nil(t, stored.IAM, "the old holder's evaluation must not land under the new fence")
}

// A takeover starts without the previous fence's evaluation, and the new holder
// records its own under the new fence.
func TestTakeoverClearsTheQuiescenceAndTheNewHolderRecordsItsOwn(t *testing.T) {
	gw := newDeleteAccountGateway(t)
	jobs, err := gw.accountDeletionStore(t.Context())
	require.NoError(t, err)
	const accountID = "000000000042"

	holderA, revisionA, err := claimAs(t, jobs, accountID, strings.Repeat("a", 32), "node-a")
	require.NoError(t, err)
	trackerA := &deletionTracker{jobs: jobs, job: holderA, revision: revisionA}
	trackerA.quiesced(t.Context(), sampleQuiescence(1))
	require.NotNil(t, describeDeletion(t, gw, accountID).IAM)

	expireLease(t, jobs, accountID)
	holderB, revisionB, err := claimAs(t, jobs, accountID, strings.Repeat("b", 32), "node-b")
	require.NoError(t, err)
	assert.Nil(t, holderB.IAM)
	assert.Nil(t, describeDeletion(t, gw, accountID).IAM, "the record shows no evaluation until the new holder makes one")

	trackerB := &deletionTracker{jobs: jobs, job: holderB, revision: revisionB}
	trackerB.quiesced(t.Context(), sampleQuiescence(2))
	stored := describeDeletion(t, gw, accountID)
	require.NotNil(t, stored.IAM)
	assert.Equal(t, uint64(2), stored.IAM.Fence)
}

// The field is optional on the wire: a record from before it decodes without it
// and prints without it, and one with it keeps the lists as arrays.
func TestAdminWire_DeletionJobIAMIsOptional(t *testing.T) {
	var old accountDeletionJob
	require.NoError(t, json.Unmarshal([]byte(`{"deletionId":"d","accountId":"000000000042","state":"RUNNING"}`), &old))
	assert.Nil(t, old.IAM)
	encoded, err := json.Marshal(old)
	require.NoError(t, err)
	assert.NotContains(t, string(encoded), `"iam"`)

	quiescence := sampleQuiescence(1)
	data, err := json.Marshal(accountDeletionJob{DeletionID: "d", AccountID: "000000000042", State: "RUNNING", IAM: &quiescence})
	require.NoError(t, err)
	var raw map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(data, &raw))
	goldenJSON(t, raw["iam"], `{
		"operationId":"op-1","fence":1,"generation":4,"quiescedScope":[],
		"unresolved":[{"scope":"ec2-key-pair","class":"writer","reason":"final-inventory-reread-not-wired"}],
		"consumers":[{"consumer":"awsgw-admission","basis":"ADR-0005:S7"}],
		"writers":[{"writer":"daemon-nats-handlers","basis":"owner-inventory","ref":"mulga-tpoz4"}],
		"inventory":[{"kind":"key-pair","status":"observed-clean-at-scan-time"}],
		"evaluatedAt":"2026-11-02T00:00:00Z"}`)
}
