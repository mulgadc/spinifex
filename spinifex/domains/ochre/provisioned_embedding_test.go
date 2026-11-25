package ochre

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/service/bedrock"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	awsidentifiers "github.com/mulgadc/spinifex/spinifex/foundation/aws/identifiers"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const systemAccount = awsidentifiers.GlobalAccountID

// embeddingGroupMembers is every catalog model whose co-serve group contains
// an embedding model: the ochre-demo bundle and standalone bge.
var embeddingGroupMembers = []string{selfHostTestModel, nomicModel, rerankerModel, bgeModel}

// seedLegacySystemCommitment writes a system-account commitment directly, as
// one created before such commitments were refused, and returns its ARN and
// ID.
func seedLegacySystemCommitment(t *testing.T, store *ProvisionedStore, modelID string) (string, string) {
	t.Helper()
	id := "legacy-" + time.Now().UTC().Format("150405.000000000")
	arn := FormatProvisionedModelARN(store.region, systemAccount, id)
	now := time.Now().UTC()
	rec := ProvisionedModelRecord{
		ARN:                  arn,
		ProvisionedModelName: "legacy",
		ModelID:              modelID,
		AccountID:            systemAccount,
		ModelUnits:           1,
		Status:               bedrock.ProvisionedModelStatusInService,
		CreationTime:         now,
		LastModifiedTime:     now,
	}
	require.NoError(t, store.store.Set(context.Background(), provisionedKey(systemAccount, id), &rec))
	return arn, id
}

func commitmentExists(t *testing.T, store *ProvisionedStore, accountID, id string) bool {
	t.Helper()
	_, found, err := store.get(context.Background(), accountID, id)
	require.NoError(t, err)
	return found
}

func deleteCommitment(store *ProvisionedStore, accountID, id string) error {
	_, err := DeleteProvisionedModelThroughput(context.Background(), accountID, store,
		&bedrock.DeleteProvisionedModelThroughputInput{ProvisionedModelId: &id})
	return err
}

func TestCreateProvisionedModelThroughput_SystemAccountRefusedForEmbeddingGroups(t *testing.T) {
	t.Parallel()
	for _, modelID := range embeddingGroupMembers {
		t.Run(modelID, func(t *testing.T) {
			t.Parallel()
			stub := newStubEndpointProvisioner()
			store := newProvisionedTestStore(t, stub)

			_, err := CreateProvisionedModelThroughput(context.Background(), systemAccount, store, createInput(modelID, "sys-pt", 1))
			require.Error(t, err)
			assert.Equal(t, awserrors.ErrorValidationException, awserrors.ValidErrorCodeFromError(err))
			assert.Empty(t, stub.ensureCalls(), "a refused create must not pin an endpoint")

			list, err := ListProvisionedModelThroughputs(context.Background(), systemAccount, store, &bedrock.ListProvisionedModelThroughputsInput{})
			require.NoError(t, err)
			assert.Empty(t, list.ProvisionedModelSummaries, "a refused create must not write a commitment")
		})
	}
}

func TestCreateProvisionedModelThroughput_SystemAccountUnrelatedGroupUnchanged(t *testing.T) {
	t.Parallel()
	stub := newStubEndpointProvisioner()
	store := newProvisionedTestStore(t, stub)

	out, err := CreateProvisionedModelThroughput(context.Background(), systemAccount, store, createInput(selfHostTestModel3B, "sys-pt", 1))
	require.NoError(t, err)
	assert.NotEmpty(t, *out.ProvisionedModelArn)
	assert.Equal(t, []provisionerCall{{accountID: systemAccount, modelID: selfHostTestModel3B}}, stub.ensureCalls())
}

func TestProvisionedModelThroughput_TenantEmbeddingGroupUnchanged(t *testing.T) {
	t.Parallel()
	for _, modelID := range embeddingGroupMembers {
		t.Run(modelID, func(t *testing.T) {
			t.Parallel()
			stub := newStubEndpointProvisioner()
			store := newProvisionedTestStore(t, stub)

			out, err := CreateProvisionedModelThroughput(context.Background(), ptCallerAccount, store, createInput(modelID, "tenant-pt", 1))
			require.NoError(t, err)
			assert.Equal(t, []provisionerCall{{accountID: ptCallerAccount, modelID: modelID}}, stub.ensureCalls())

			stub.setState(ptCallerAccount, modelID, string(StateReady))
			require.NoError(t, deleteCommitment(store, ptCallerAccount, *out.ProvisionedModelArn))
			assert.Equal(t, []provisionerCall{{accountID: ptCallerAccount, modelID: modelID}}, stub.deleteCalls(),
				"a tenant commitment still tears down its own endpoint")
		})
	}
}

func TestDeleteProvisionedModelThroughput_LegacySystemConflictsWhileSharedEndpointExists(t *testing.T) {
	t.Parallel()
	for _, modelID := range embeddingGroupMembers {
		for _, state := range []EndpointState{StateStarting, StateReady, StateDraining} {
			t.Run(modelID+"/"+string(state), func(t *testing.T) {
				t.Parallel()
				stub := newStubEndpointProvisioner()
				store := newProvisionedTestStore(t, stub)
				_, id := seedLegacySystemCommitment(t, store, modelID)
				stub.setState(systemAccount, modelID, string(state))

				err := deleteCommitment(store, systemAccount, id)
				require.Error(t, err)
				assert.Equal(t, awserrors.ErrorConflictException, awserrors.ValidErrorCodeFromError(err))
				assert.Contains(t, err.Error(), "spx admin ochre endpoint delete")
				assert.Empty(t, stub.deleteCalls(), "the shared endpoint is never torn down from here")
				assert.Equal(t, string(state), stub.stateOf(systemAccount, modelID))
				assert.True(t, commitmentExists(t, store, systemAccount, id))
			})
		}
	}
}

// With the shared endpoint absent only the record goes, and an endpoint an
// operator starts between that check and the removal is left alone.
func TestDeleteProvisionedModelThroughput_LegacySystemAbsentRemovesRecordOnly(t *testing.T) {
	t.Parallel()
	for _, modelID := range embeddingGroupMembers {
		t.Run(modelID, func(t *testing.T) {
			t.Parallel()
			stub := newStubEndpointProvisioner()
			store := newProvisionedTestStore(t, stub)
			_, id := seedLegacySystemCommitment(t, store, modelID)
			stub.setState(systemAccount, modelID, string(StateAbsent))
			stub.afterEndpointState = func() { stub.setState(systemAccount, modelID, string(StateStarting)) }

			require.NoError(t, deleteCommitment(store, systemAccount, id))
			assert.False(t, commitmentExists(t, store, systemAccount, id))
			assert.Empty(t, stub.deleteCalls())
			assert.Equal(t, string(StateStarting), stub.stateOf(systemAccount, modelID), "the operator's new endpoint survives")
		})
	}
}

func TestDeleteProvisionedModelThroughput_LegacySystemUnknownGroupFailsClosed(t *testing.T) {
	t.Parallel()
	stub := newStubEndpointProvisioner()
	store := newProvisionedTestStore(t, stub)
	_, id := seedLegacySystemCommitment(t, store, "retired.model-v1:0")

	err := deleteCommitment(store, systemAccount, id)
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorConflictException, awserrors.ValidErrorCodeFromError(err))
	assert.Empty(t, stub.deleteCalls())
	assert.True(t, commitmentExists(t, store, systemAccount, id))
}

func TestDeleteProvisionedModelThroughput_LegacySystemUnrelatedGroupUnchanged(t *testing.T) {
	t.Parallel()
	stub := newStubEndpointProvisioner()
	store := newProvisionedTestStore(t, stub)
	_, id := seedLegacySystemCommitment(t, store, selfHostTestModel3B)
	stub.setState(systemAccount, selfHostTestModel3B, string(StateReady))

	require.NoError(t, deleteCommitment(store, systemAccount, id))
	assert.Equal(t, []provisionerCall{{accountID: systemAccount, modelID: selfHostTestModel3B}}, stub.deleteCalls())
	assert.False(t, commitmentExists(t, store, systemAccount, id))
}

// runtimeOps invokes each of the four runtime operations for modelID.
func runtimeOps(rt *Router, accountID, modelID string) map[string]func() error {
	ctx := context.Background()
	return map[string]func() error{
		"InvokeModel": func() error {
			_, _, err := rt.InvokeModel(ctx, accountID, modelID, []byte(`{"prompt":"hello"}`), "", "")
			return err
		},
		"InvokeModelWithResponseStream": func() error {
			_, err := rt.InvokeModelWithResponseStream(ctx, accountID, modelID, []byte(`{"prompt":"hello"}`), "", "")
			return err
		},
		"Converse": func() error {
			_, err := rt.Converse(ctx, accountID, modelID, converseInput())
			return err
		},
		"ConverseStream": func() error {
			_, err := rt.ConverseStream(ctx, accountID, modelID, converseStreamInput())
			return err
		},
	}
}

func TestRouter_LegacySystemCommitmentRefusedOnAllOperations(t *testing.T) {
	t.Parallel()
	type opCase struct {
		modelID string
		access  AccessResolver
		want    string
	}
	cases := []opCase{
		// A retired model no longer resolves in the catalog, so the access
		// check refuses it before the commitment is examined.
		{"retired.model-v1:0", grantAll{}, awserrors.ErrorResourceNotFoundException},
	}
	for _, m := range embeddingGroupMembers {
		cases = append(cases,
			opCase{m, grantAll{}, awserrors.ErrorValidationException},
			opCase{m, grantSet{}, awserrors.ErrorAccessDeniedException})
	}
	for _, tc := range cases {
		store := newProvisionedTestStore(t, newStubEndpointProvisioner())
		arn, _ := seedLegacySystemCommitment(t, store, tc.modelID)

		spy := &spyEndpointResolver{baseURL: "http://unused:1"}
		rt := NewRouter(nil, spy, &fakeRecorder{}, tc.access, store, nil, nil)
		for op, call := range runtimeOps(rt, systemAccount, arn) {
			t.Run(tc.modelID+"/"+tc.want+"/"+op, func(t *testing.T) {
				err := call()
				require.Error(t, err)
				assert.Equal(t, tc.want, err.Error())
			})
		}
		assert.Empty(t, spy.endpointCalls, tc.modelID)
		assert.Empty(t, spy.endpointForAccountCalls, tc.modelID)
	}
}

func TestRouter_LegacySystemCommitmentUnrelatedGroupStillServes(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices": [{"text": "hi", "finish_reason": "stop"}], "usage": {"prompt_tokens": 1, "completion_tokens": 1}}`))
	}))
	t.Cleanup(ts.Close)

	store := newProvisionedTestStore(t, newStubEndpointProvisioner())
	arn, _ := seedLegacySystemCommitment(t, store, selfHostTestModel3B)
	spy := &spyEndpointResolver{baseURL: ts.URL}
	rt := NewRouter(nil, spy, &fakeRecorder{}, grantAll{}, store, nil, nil)

	_, _, err := rt.InvokeModel(context.Background(), systemAccount, arn, []byte(`{"prompt":"hello"}`), "", "")
	require.NoError(t, err)
	require.Len(t, spy.endpointForAccountCalls, 1)
	assert.Equal(t, systemAccount, spy.endpointForAccountCalls[0].accountID)
}

// An invocation through the commitment while its absent-endpoint cleanup is
// in progress is refused, so it cannot start a pinned shared endpoint that
// outlives the record.
func TestRouter_LegacySystemCommitmentInvokeRacingCleanupIsRefused(t *testing.T) {
	stub := newStubEndpointProvisioner()
	store := newProvisionedTestStore(t, stub)
	arn, id := seedLegacySystemCommitment(t, store, selfHostTestModel)
	stub.setState(systemAccount, selfHostTestModel, string(StateAbsent))

	spy := &spyEndpointResolver{baseURL: "http://unused:1"}
	rt := NewRouter(nil, spy, &fakeRecorder{}, grantAll{}, store, nil, nil)

	var raced int
	stub.afterEndpointState = func() {
		for _, call := range runtimeOps(rt, systemAccount, arn) {
			raced++
			err := call()
			require.Error(t, err)
			assert.Equal(t, awserrors.ErrorValidationException, err.Error())
		}
	}

	require.NoError(t, deleteCommitment(store, systemAccount, id))
	assert.Equal(t, 4, raced)
	assert.False(t, commitmentExists(t, store, systemAccount, id))
	assert.Empty(t, spy.endpointCalls)
	assert.Empty(t, spy.endpointForAccountCalls)
	assert.Empty(t, stub.ensureCalls())
	assert.Empty(t, stub.deleteCalls())
	assert.Equal(t, string(StateAbsent), stub.stateOf(systemAccount, selfHostTestModel))
}
