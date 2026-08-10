package handlers_bedrock

import (
	"context"

	gateway_bedrock "github.com/mulgadc/spinifex/spinifex/gateway/bedrock"
)

// ProvisionedEndpointAdapter adapts EndpointService to gateway_bedrock's
// EndpointProvisioner, the narrow surface the provisioned-throughput ops
// need. It exists because gateway_bedrock cannot import this package back
// (handlers_bedrock already imports gateway_bedrock for LookupServingSpec),
// so EndpointProvisioner is declared there with primitive-typed methods and
// this adapter is what actually calls EndpointService underneath.
type ProvisionedEndpointAdapter struct {
	svc EndpointService
}

var _ gateway_bedrock.EndpointProvisioner = (*ProvisionedEndpointAdapter)(nil)

// NewProvisionedEndpointAdapter constructs an adapter over svc.
func NewProvisionedEndpointAdapter(svc EndpointService) *ProvisionedEndpointAdapter {
	return &ProvisionedEndpointAdapter{svc: svc}
}

// EnsurePinned requests a pinned, account-scoped endpoint for modelID.
func (a *ProvisionedEndpointAdapter) EnsurePinned(ctx context.Context, accountID, modelID string) error {
	_, err := a.svc.Ensure(ctx, &EnsureEndpointInput{ModelID: modelID, AccountID: accountID, Pinned: true}, accountID)
	return err
}

// EndpointState reports (accountID, modelID)'s current endpoint state
// ("STARTING", "READY", "DRAINING", or "ABSENT"). Never an error for a model
// with no endpoint at all: Describe already treats that as a normal answer.
func (a *ProvisionedEndpointAdapter) EndpointState(ctx context.Context, accountID, modelID string) (string, error) {
	out, err := a.svc.Describe(ctx, &DescribeEndpointInput{ModelID: modelID, AccountID: accountID}, accountID)
	if err != nil {
		return "", err
	}
	return string(out.Endpoint.State), nil
}

// DeletePinned tears down (accountID, modelID)'s pinned endpoint. Idempotent:
// an already-absent endpoint is a success.
func (a *ProvisionedEndpointAdapter) DeletePinned(ctx context.Context, accountID, modelID string) error {
	_, err := a.svc.Delete(ctx, &DeleteEndpointInput{ModelID: modelID, AccountID: accountID}, accountID)
	return err
}
