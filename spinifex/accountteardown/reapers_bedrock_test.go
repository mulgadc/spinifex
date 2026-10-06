package accountteardown

//test:in-package — the reaper is unexported, and the account filter it applies
// to a cluster-wide listing is the substance of it.

import (
	"context"
	"errors"
	"testing"

	"github.com/mulgadc/spinifex/spinifex/domains/ochre"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type fakeEndpoints struct {
	ochre.EndpointService

	records []ochre.EndpointRecord
	deleted []ochre.DeleteEndpointInput
	err     error
}

func (f *fakeEndpoints) List(_ context.Context, _ *ochre.ListEndpointsInput, _ string) (*ochre.ListEndpointsOutput, error) {
	return &ochre.ListEndpointsOutput{Endpoints: f.records}, nil
}

func (f *fakeEndpoints) Delete(_ context.Context, in *ochre.DeleteEndpointInput, _ string) (*ochre.DeleteEndpointOutput, error) {
	if f.err != nil {
		return nil, f.err
	}
	f.deleted = append(f.deleted, *in)
	return &ochre.DeleteEndpointOutput{}, nil
}

// Bedrock's List answers with every endpoint in the cluster whatever account
// it is called for, so the filter is the reaper's own responsibility: without
// it a tenant teardown would delete the shared endpoint and other tenants'.
func TestBedrockEndpointReaperListsOnlyItsOwnAccount(t *testing.T) {
	svc := &fakeEndpoints{records: []ochre.EndpointRecord{
		{AccountID: "000000000002", ModelID: "mistral-7b", State: ochre.StateReady},
		{AccountID: "000000000003", ModelID: "llama-3", State: ochre.StateReady},
		{AccountID: "global", ModelID: "shared", State: ochre.StateReady},
		{AccountID: "000000000002", ModelID: "", State: ochre.StateStarting},
	}}
	reaper := &bedrockEndpointReaper{svc: svc}

	assert.Equal(t, StageCompute, reaper.Stage())

	found, err := reaper.List(t.Context(), "000000000002")
	require.NoError(t, err)
	require.Len(t, found, 1)
	assert.Equal(t, "mistral-7b", found[0].ID)
	assert.Equal(t, "READY", found[0].Detail)
}

// An empty AccountID resolves to the global account on the far side, so a
// delete that leaves it unset removes the shared endpoint instead.
func TestBedrockEndpointReaperNamesTheAccountItDeletesFor(t *testing.T) {
	svc := &fakeEndpoints{}
	reaper := &bedrockEndpointReaper{svc: svc}

	require.NoError(t, reaper.Delete(t.Context(), "000000000002", Resource{ID: "mistral-7b"}, false))
	require.Len(t, svc.deleted, 1)
	assert.Equal(t, "000000000002", svc.deleted[0].AccountID)
	assert.Equal(t, "mistral-7b", svc.deleted[0].ModelID)
}

func TestBedrockEndpointReaperTreatsAMissingEndpointAsDeleted(t *testing.T) {
	svc := &fakeEndpoints{err: errors.New("ResourceNotFoundException: no such endpoint")}
	reaper := &bedrockEndpointReaper{svc: svc}

	assert.NoError(t, reaper.Delete(t.Context(), "000000000002", Resource{ID: "mistral-7b"}, false))
}
