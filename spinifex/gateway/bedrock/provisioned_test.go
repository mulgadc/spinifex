package gateway_bedrock

import (
	"context"
	"sync"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/bedrock"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	"github.com/mulgadc/spinifex/spinifex/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// stubEndpointProvisioner is an in-memory EndpointProvisioner: it records
// every call so a test can assert how the PT ops drove it, and its state per
// (accountID, modelID) is a plain map a test seeds directly rather than a
// real endpoint lifecycle, standing in for the daemon.
type stubEndpointProvisioner struct {
	mu sync.Mutex

	ensured []provisionerCall
	deleted []provisionerCall

	// state maps "accountID/modelID" to the endpoint state EndpointState
	// reports; an absent entry reports "" (ABSENT), matching
	// handlers_bedrock.StateAbsent's zero-value shape.
	state map[string]string

	ensureErr error
	deleteErr error
}

type provisionerCall struct {
	accountID string
	modelID   string
}

func newStubEndpointProvisioner() *stubEndpointProvisioner {
	return &stubEndpointProvisioner{state: map[string]string{}}
}

func provisionerStateKey(accountID, modelID string) string {
	return accountID + "/" + modelID
}

func (s *stubEndpointProvisioner) setState(accountID, modelID, state string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.state[provisionerStateKey(accountID, modelID)] = state
}

func (s *stubEndpointProvisioner) EnsurePinned(_ context.Context, accountID, modelID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ensured = append(s.ensured, provisionerCall{accountID: accountID, modelID: modelID})
	if s.ensureErr != nil {
		return s.ensureErr
	}
	if _, ok := s.state[provisionerStateKey(accountID, modelID)]; !ok {
		s.state[provisionerStateKey(accountID, modelID)] = endpointStateStarting
	}
	return nil
}

func (s *stubEndpointProvisioner) EndpointState(_ context.Context, accountID, modelID string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.state[provisionerStateKey(accountID, modelID)], nil
}

func (s *stubEndpointProvisioner) DeletePinned(_ context.Context, accountID, modelID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.deleted = append(s.deleted, provisionerCall{accountID: accountID, modelID: modelID})
	if s.deleteErr != nil {
		return s.deleteErr
	}
	delete(s.state, provisionerStateKey(accountID, modelID))
	return nil
}

func (s *stubEndpointProvisioner) ensureCalls() []provisionerCall {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]provisionerCall(nil), s.ensured...)
}

func (s *stubEndpointProvisioner) deleteCalls() []provisionerCall {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]provisionerCall(nil), s.deleted...)
}

var _ EndpointProvisioner = (*stubEndpointProvisioner)(nil)

const (
	ptCallerAccount = "000000000001"
	ptOtherCaller   = "000000000002"
)

func newProvisionedTestStore(t *testing.T, endpoint EndpointProvisioner) *ProvisionedStore {
	t.Helper()
	_, _, js := testutil.StartTestJetStream(t)
	return NewProvisionedStore(js, 1, ptTestRegion, endpoint)
}

func createInput(modelID, name string, units int64) *bedrock.CreateProvisionedModelThroughputInput {
	return &bedrock.CreateProvisionedModelThroughputInput{
		ModelId:              aws.String(modelID),
		ProvisionedModelName: aws.String(name),
		ModelUnits:           aws.Int64(units),
	}
}

// TestCreateProvisionedModelThroughput_RejectsProviderHostedModel guards the
// bead's core constraint: provisioning capacity only makes sense for a model
// this platform actually launches VMs for.
func TestCreateProvisionedModelThroughput_RejectsProviderHostedModel(t *testing.T) {
	withProviderCatalogEntry(t, anthropicTestModel)
	stub := newStubEndpointProvisioner()
	store := newProvisionedTestStore(t, stub)

	_, err := CreateProvisionedModelThroughput(context.Background(), ptCallerAccount, store,
		createInput(anthropicTestModel, "my-pt", 1))
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorResourceNotFoundException, err.Error())
	assert.Empty(t, stub.ensureCalls(), "a rejected create must never touch the endpoint lifecycle")
}

// TestCreateProvisionedModelThroughput_RejectsUnknownModel covers the other
// LookupServingSpec refusal: a model absent from the catalog entirely.
func TestCreateProvisionedModelThroughput_RejectsUnknownModel(t *testing.T) {
	stub := newStubEndpointProvisioner()
	store := newProvisionedTestStore(t, stub)

	_, err := CreateProvisionedModelThroughput(context.Background(), ptCallerAccount, store,
		createInput("not-a-real-model", "my-pt", 1))
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorResourceNotFoundException, err.Error())
}

// TestCreateProvisionedModelThroughput_SelfHostSucceeds is the happy path:
// Ensure is called for the caller's own account, the record is written
// Creating, and a well-formed, parseable ARN comes back.
func TestCreateProvisionedModelThroughput_SelfHostSucceeds(t *testing.T) {
	stub := newStubEndpointProvisioner()
	store := newProvisionedTestStore(t, stub)
	ctx := context.Background()

	out, err := CreateProvisionedModelThroughput(ctx, ptCallerAccount, store,
		createInput(selfHostTestModel, "my-pt", 2))
	require.NoError(t, err)
	require.NotNil(t, out.ProvisionedModelArn)

	arn := aws.StringValue(out.ProvisionedModelArn)
	parsed, err := ParseProvisionedModelARN(arn, ptTestRegion, ptCallerAccount)
	require.NoError(t, err, "Create must return a well-formed, self-parseable ARN")
	assert.NotEmpty(t, parsed.ID)

	calls := stub.ensureCalls()
	require.Len(t, calls, 1)
	assert.Equal(t, ptCallerAccount, calls[0].accountID, "Ensure must be keyed on the caller's account, not the shared platform account")
	assert.Equal(t, selfHostTestModel, calls[0].modelID)

	getOut, err := GetProvisionedModelThroughput(ctx, ptCallerAccount, store,
		&bedrock.GetProvisionedModelThroughputInput{ProvisionedModelId: aws.String(parsed.ID)})
	require.NoError(t, err)
	assert.Equal(t, bedrock.ProvisionedModelStatusCreating, aws.StringValue(getOut.Status),
		"a just-created commitment reads Creating because the stub endpoint starts STARTING")
	assert.Equal(t, "my-pt", aws.StringValue(getOut.ProvisionedModelName))
	assert.Equal(t, int64(2), aws.Int64Value(getOut.ModelUnits))
}

// TestGetProvisionedModelThroughput_StatusDerivation pins the exact mapping
// the plan specifies: STARTING->Creating, READY->InService, absent->Failed.
func TestGetProvisionedModelThroughput_StatusDerivation(t *testing.T) {
	cases := []struct {
		name       string
		state      string
		wantStatus string
	}{
		{"starting", endpointStateStarting, bedrock.ProvisionedModelStatusCreating},
		{"ready", endpointStateReady, bedrock.ProvisionedModelStatusInService},
		{"absent", "", bedrock.ProvisionedModelStatusFailed},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			stub := newStubEndpointProvisioner()
			store := newProvisionedTestStore(t, stub)
			ctx := context.Background()

			out, err := CreateProvisionedModelThroughput(ctx, ptCallerAccount, store,
				createInput(selfHostTestModel, "my-pt", 1))
			require.NoError(t, err)
			parsed, err := ParseProvisionedModelARN(aws.StringValue(out.ProvisionedModelArn), ptTestRegion, ptCallerAccount)
			require.NoError(t, err)

			stub.setState(ptCallerAccount, selfHostTestModel, tc.state)

			getOut, err := GetProvisionedModelThroughput(ctx, ptCallerAccount, store,
				&bedrock.GetProvisionedModelThroughputInput{ProvisionedModelId: aws.String(parsed.ID)})
			require.NoError(t, err)
			assert.Equal(t, tc.wantStatus, aws.StringValue(getOut.Status))
		})
	}
}

// TestGetProvisionedModelThroughput_NotFound covers both a bare id that was
// never created and a foreign account presenting its own accountID: both
// must read as not-found, never leaking whether the id exists elsewhere.
func TestGetProvisionedModelThroughput_NotFound(t *testing.T) {
	stub := newStubEndpointProvisioner()
	store := newProvisionedTestStore(t, stub)
	ctx := context.Background()

	out, err := CreateProvisionedModelThroughput(ctx, ptCallerAccount, store,
		createInput(selfHostTestModel, "my-pt", 1))
	require.NoError(t, err)
	parsed, err := ParseProvisionedModelARN(aws.StringValue(out.ProvisionedModelArn), ptTestRegion, ptCallerAccount)
	require.NoError(t, err)

	_, err = GetProvisionedModelThroughput(ctx, ptOtherCaller, store,
		&bedrock.GetProvisionedModelThroughputInput{ProvisionedModelId: aws.String(parsed.ID)})
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorResourceNotFoundException, err.Error())

	_, err = GetProvisionedModelThroughput(ctx, ptCallerAccount, store,
		&bedrock.GetProvisionedModelThroughputInput{ProvisionedModelId: aws.String("does-not-exist")})
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorResourceNotFoundException, err.Error())
}

// TestListProvisionedModelThroughputs_AccountScoped is the plan's explicit
// isolation requirement: account B never sees account A's commitment.
func TestListProvisionedModelThroughputs_AccountScoped(t *testing.T) {
	stub := newStubEndpointProvisioner()
	store := newProvisionedTestStore(t, stub)
	ctx := context.Background()

	_, err := CreateProvisionedModelThroughput(ctx, ptCallerAccount, store,
		createInput(selfHostTestModel, "acct-a-pt", 1))
	require.NoError(t, err)

	listA, err := ListProvisionedModelThroughputs(ctx, ptCallerAccount, store, nil)
	require.NoError(t, err)
	require.Len(t, listA.ProvisionedModelSummaries, 1)
	assert.Equal(t, "acct-a-pt", aws.StringValue(listA.ProvisionedModelSummaries[0].ProvisionedModelName))

	listB, err := ListProvisionedModelThroughputs(ctx, ptOtherCaller, store, nil)
	require.NoError(t, err)
	assert.Empty(t, listB.ProvisionedModelSummaries, "account B must not see account A's commitment")
}

// TestUpdateProvisionedModelThroughput_RejectsModelSwap pins the resolved
// decision: Update honours name/tags only, never a served-model change.
func TestUpdateProvisionedModelThroughput_RejectsModelSwap(t *testing.T) {
	stub := newStubEndpointProvisioner()
	store := newProvisionedTestStore(t, stub)
	ctx := context.Background()

	out, err := CreateProvisionedModelThroughput(ctx, ptCallerAccount, store,
		createInput(selfHostTestModel, "my-pt", 1))
	require.NoError(t, err)
	parsed, err := ParseProvisionedModelARN(aws.StringValue(out.ProvisionedModelArn), ptTestRegion, ptCallerAccount)
	require.NoError(t, err)

	_, err = UpdateProvisionedModelThroughput(ctx, ptCallerAccount, store,
		&bedrock.UpdateProvisionedModelThroughputInput{
			ProvisionedModelId: aws.String(parsed.ID),
			DesiredModelId:     aws.String(selfHostTestModel3B),
		})
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorValidationException, awserrors.ValidErrorCodeFromError(err))
}

// TestUpdateProvisionedModelThroughput_AcceptsNameChange covers the mutable
// side of the same op: a rename must succeed and persist.
func TestUpdateProvisionedModelThroughput_AcceptsNameChange(t *testing.T) {
	stub := newStubEndpointProvisioner()
	store := newProvisionedTestStore(t, stub)
	ctx := context.Background()

	out, err := CreateProvisionedModelThroughput(ctx, ptCallerAccount, store,
		createInput(selfHostTestModel, "old-name", 1))
	require.NoError(t, err)
	parsed, err := ParseProvisionedModelARN(aws.StringValue(out.ProvisionedModelArn), ptTestRegion, ptCallerAccount)
	require.NoError(t, err)

	_, err = UpdateProvisionedModelThroughput(ctx, ptCallerAccount, store,
		&bedrock.UpdateProvisionedModelThroughputInput{
			ProvisionedModelId:          aws.String(parsed.ID),
			DesiredProvisionedModelName: aws.String("new-name"),
		})
	require.NoError(t, err)

	getOut, err := GetProvisionedModelThroughput(ctx, ptCallerAccount, store,
		&bedrock.GetProvisionedModelThroughputInput{ProvisionedModelId: aws.String(parsed.ID)})
	require.NoError(t, err)
	assert.Equal(t, "new-name", aws.StringValue(getOut.ProvisionedModelName))
}

// TestDeleteProvisionedModelThroughput_RemovesEndpointAndRecord covers both
// halves of Delete: the daemon call for the right account+model, and the KV
// record actually going away (a subsequent Get reads not-found).
func TestDeleteProvisionedModelThroughput_RemovesEndpointAndRecord(t *testing.T) {
	stub := newStubEndpointProvisioner()
	store := newProvisionedTestStore(t, stub)
	ctx := context.Background()

	out, err := CreateProvisionedModelThroughput(ctx, ptCallerAccount, store,
		createInput(selfHostTestModel, "my-pt", 1))
	require.NoError(t, err)
	parsed, err := ParseProvisionedModelARN(aws.StringValue(out.ProvisionedModelArn), ptTestRegion, ptCallerAccount)
	require.NoError(t, err)

	_, err = DeleteProvisionedModelThroughput(ctx, ptCallerAccount, store,
		&bedrock.DeleteProvisionedModelThroughputInput{ProvisionedModelId: aws.String(parsed.ID)})
	require.NoError(t, err)

	deletes := stub.deleteCalls()
	require.Len(t, deletes, 1)
	assert.Equal(t, ptCallerAccount, deletes[0].accountID)
	assert.Equal(t, selfHostTestModel, deletes[0].modelID)

	_, err = GetProvisionedModelThroughput(ctx, ptCallerAccount, store,
		&bedrock.GetProvisionedModelThroughputInput{ProvisionedModelId: aws.String(parsed.ID)})
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorResourceNotFoundException, err.Error())
}

// TestDeleteProvisionedModelThroughput_AbsentIsNoop mirrors
// handlers_bedrock.Service.Delete's own idempotence: deleting an
// already-absent commitment must succeed, not error.
func TestDeleteProvisionedModelThroughput_AbsentIsNoop(t *testing.T) {
	stub := newStubEndpointProvisioner()
	store := newProvisionedTestStore(t, stub)

	_, err := DeleteProvisionedModelThroughput(context.Background(), ptCallerAccount, store,
		&bedrock.DeleteProvisionedModelThroughputInput{ProvisionedModelId: aws.String("never-created")})
	require.NoError(t, err)
	assert.Empty(t, stub.deleteCalls(), "an absent record must never reach the endpoint lifecycle")
}

// TestCommittedModelUnits_RejectsAnUndecodableRecord asserts a corrupt record
// fails the sum rather than being skipped. Skipping undercounts committed
// capacity, which admits traffic the commitments do not cover.
func TestCommittedModelUnits_RejectsAnUndecodableRecord(t *testing.T) {
	store := newProvisionedTestStore(t, newStubEndpointProvisioner())
	ctx := context.Background()

	_, err := CreateProvisionedModelThroughput(ctx, ptCallerAccount, store, createInput(selfHostTestModel, "readable", 2))
	require.NoError(t, err)

	kv, err := store.store.KV(ctx)
	require.NoError(t, err)
	_, err = kv.Put(ctx, provisionedKey(ptCallerAccount, "corrupt"), []byte("{not json"))
	require.NoError(t, err)

	_, err = committedModelUnits(ctx, store, ptCallerAccount, selfHostTestModel)
	require.Error(t, err)
}

// TestProvisionedStore_UpdateRejectsAStaleRevision covers the CAS guard: the
// second writer at the same revision loses, and reports a retryable
// ConflictException rather than clobbering the winner.
func TestProvisionedStore_UpdateRejectsAStaleRevision(t *testing.T) {
	store := newProvisionedTestStore(t, newStubEndpointProvisioner())
	ctx := context.Background()

	out, err := CreateProvisionedModelThroughput(ctx, ptCallerAccount, store, createInput(selfHostTestModel, "cas", 1))
	require.NoError(t, err)
	parsed, err := ParseProvisionedModelARN(aws.StringValue(out.ProvisionedModelArn), ptTestRegion, ptCallerAccount)
	require.NoError(t, err)
	key := provisionedKey(ptCallerAccount, parsed.ID)

	rec, stale, found, err := store.getRevision(ctx, key)
	require.NoError(t, err)
	require.True(t, found)

	rec.ProvisionedModelName = "first-writer"
	require.NoError(t, store.update(ctx, key, rec, stale))

	rec.ProvisionedModelName = "second-writer"
	err = store.update(ctx, key, rec, stale)
	require.Error(t, err)
	assert.True(t, awserrors.IsErrorCode(err, awserrors.ErrorConflictException))

	got, _, _, err := store.getRevision(ctx, key)
	require.NoError(t, err)
	assert.Equal(t, "first-writer", got.ProvisionedModelName, "the loser must not have clobbered the winner")
}
