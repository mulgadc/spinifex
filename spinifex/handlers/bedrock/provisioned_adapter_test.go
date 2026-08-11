package handlers_bedrock

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// recordingEndpointService is an EndpointService that captures the exact
// input of its last Ensure/Describe/Delete call, so the adapter test can
// assert the translation to EndpointProvisioner's primitive-typed methods
// carries AccountID/Pinned through unchanged rather than just that some call
// happened.
type recordingEndpointService struct {
	lastEnsure   *EnsureEndpointInput
	lastDescribe *DescribeEndpointInput
	lastDelete   *DeleteEndpointInput

	describeOut EndpointRecord
	ensureErr   error
	describeErr error
	deleteErr   error
}

var _ EndpointService = (*recordingEndpointService)(nil)

func (r *recordingEndpointService) Ensure(_ context.Context, in *EnsureEndpointInput, _ string) (*EnsureEndpointOutput, error) {
	r.lastEnsure = in
	if r.ensureErr != nil {
		return nil, r.ensureErr
	}
	return &EnsureEndpointOutput{}, nil
}

func (r *recordingEndpointService) Describe(_ context.Context, in *DescribeEndpointInput, _ string) (*DescribeEndpointOutput, error) {
	r.lastDescribe = in
	if r.describeErr != nil {
		return nil, r.describeErr
	}
	return &DescribeEndpointOutput{Endpoint: r.describeOut}, nil
}

func (r *recordingEndpointService) List(context.Context, *ListEndpointsInput, string) (*ListEndpointsOutput, error) {
	return &ListEndpointsOutput{}, nil
}

func (r *recordingEndpointService) Delete(_ context.Context, in *DeleteEndpointInput, _ string) (*DeleteEndpointOutput, error) {
	r.lastDelete = in
	if r.deleteErr != nil {
		return nil, r.deleteErr
	}
	return &DeleteEndpointOutput{}, nil
}

const adapterTestModel = "meta.llama3-2-1b-instruct-v1:0"
const adapterTestAccount = "000000000001"

// TestProvisionedEndpointAdapter_EnsurePinned_AlwaysPinsAndScopesAccount
// guards the one thing the whole PT feature depends on: every endpoint it
// launches is pinned (exempt from idle reaping/eviction) and keyed to the
// caller's real account, never the shared platform account.
func TestProvisionedEndpointAdapter_EnsurePinned_AlwaysPinsAndScopesAccount(t *testing.T) {
	svc := &recordingEndpointService{}
	adapter := NewProvisionedEndpointAdapter(svc)

	require.NoError(t, adapter.EnsurePinned(context.Background(), adapterTestAccount, adapterTestModel))

	require.NotNil(t, svc.lastEnsure)
	assert.Equal(t, adapterTestModel, svc.lastEnsure.ModelID)
	assert.Equal(t, adapterTestAccount, svc.lastEnsure.AccountID)
	assert.True(t, svc.lastEnsure.Pinned, "every PT-launched endpoint must be pinned")
}

func TestProvisionedEndpointAdapter_EnsurePinned_PropagatesError(t *testing.T) {
	svc := &recordingEndpointService{ensureErr: errors.New("no capacity")}
	adapter := NewProvisionedEndpointAdapter(svc)

	err := adapter.EnsurePinned(context.Background(), adapterTestAccount, adapterTestModel)
	require.Error(t, err)
	assert.Equal(t, "no capacity", err.Error())
}

// TestProvisionedEndpointAdapter_EndpointState_MapsRecordState covers the
// state values Get's status derivation switches on, plus the absent case
// (ABSENT is the zero value of EndpointState's own type).
func TestProvisionedEndpointAdapter_EndpointState_MapsRecordState(t *testing.T) {
	cases := []struct {
		name  string
		state EndpointState
		want  string
	}{
		{"starting", StateStarting, "STARTING"},
		{"ready", StateReady, "READY"},
		{"draining", StateDraining, "DRAINING"},
		{"absent", StateAbsent, "ABSENT"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			svc := &recordingEndpointService{describeOut: EndpointRecord{ModelID: adapterTestModel, State: tc.state}}
			adapter := NewProvisionedEndpointAdapter(svc)

			state, err := adapter.EndpointState(context.Background(), adapterTestAccount, adapterTestModel)
			require.NoError(t, err)
			assert.Equal(t, tc.want, state)

			require.NotNil(t, svc.lastDescribe)
			assert.Equal(t, adapterTestAccount, svc.lastDescribe.AccountID, "the describe must be scoped to the caller's account")
		})
	}
}

func TestProvisionedEndpointAdapter_DeletePinned_ScopesAccount(t *testing.T) {
	svc := &recordingEndpointService{}
	adapter := NewProvisionedEndpointAdapter(svc)

	require.NoError(t, adapter.DeletePinned(context.Background(), adapterTestAccount, adapterTestModel))

	require.NotNil(t, svc.lastDelete)
	assert.Equal(t, adapterTestModel, svc.lastDelete.ModelID)
	assert.Equal(t, adapterTestAccount, svc.lastDelete.AccountID)
}
