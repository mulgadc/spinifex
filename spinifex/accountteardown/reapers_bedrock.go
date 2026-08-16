package accountteardown

import (
	"context"

	handlers_bedrock "github.com/mulgadc/spinifex/spinifex/handlers/bedrock"
	"github.com/nats-io/nats.go"
)

// BedrockReapers returns the model endpoint reaper.
//
// Compute stage, ahead of the instance reaper: an endpoint owns a VM, an ENI
// and a weights volume, and only its own delete releases all three. Terminating
// its instance first would strand the other two with no record naming them.
func BedrockReapers(nc *nats.Conn) []Reaper {
	return []Reaper{&bedrockEndpointReaper{svc: handlers_bedrock.NewNATSEndpointService(nc)}}
}

type bedrockEndpointReaper struct {
	svc handlers_bedrock.EndpointService
}

func (r *bedrockEndpointReaper) Kind() string { return "bedrock-endpoint" }
func (r *bedrockEndpointReaper) Stage() Stage { return StageCompute }

// List filters by account itself. Bedrock's List returns every endpoint in the
// cluster whatever account it is called for, so an unfiltered reaper would
// report — and then delete — endpoints belonging to other tenants and to the
// shared global account.
func (r *bedrockEndpointReaper) List(ctx context.Context, accountID string) ([]Resource, error) {
	out, err := r.svc.List(ctx, &handlers_bedrock.ListEndpointsInput{}, accountID)
	if err != nil {
		return nil, err
	}

	var found []Resource
	for _, record := range out.Endpoints {
		if record.AccountID != accountID || record.ModelID == "" {
			continue
		}
		found = append(found, Resource{
			Kind:   r.Kind(),
			ID:     record.ModelID,
			Detail: string(record.State),
		})
	}
	return found, nil
}

// Delete names the account explicitly: an empty AccountID resolves to the
// global one, which would delete the shared endpoint instead of the tenant's.
func (r *bedrockEndpointReaper) Delete(ctx context.Context, accountID string, resource Resource, _ bool) error {
	_, err := r.svc.Delete(ctx, &handlers_bedrock.DeleteEndpointInput{
		ModelID:   resource.ID,
		AccountID: accountID,
	}, accountID)
	return ignoreAlreadyGone(err)
}
