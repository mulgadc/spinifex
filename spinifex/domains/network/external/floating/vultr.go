package floating

import (
	"context"
	"errors"
	"fmt"

	"github.com/mulgadc/spinifex/spinifex/providers/cloud/vultr"
)

// vultrAPI is the slice of the Vultr client the adapter calls, so tests can
// stand the fake in for it.
type vultrAPI interface {
	CreateReservedIP(ctx context.Context, label string) (vultr.ReservedIP, error)
	GetReservedIP(ctx context.Context, id string) (vultr.ReservedIP, error)
	ListReservedIPs(ctx context.Context) ([]vultr.ReservedIP, error)
	AttachReservedIP(ctx context.Context, id, instanceID string) error
	DetachReservedIP(ctx context.Context, id string) error
	DeleteReservedIP(ctx context.Context, id string) error
}

var (
	_ vultrAPI = (*vultr.Client)(nil)
	_ vultrAPI = (*vultr.Fake)(nil)
)

// VultrProvider adapts Vultr reserved IPs to Provider.
type VultrProvider struct {
	api vultrAPI
}

var _ Provider = (*VultrProvider)(nil)

// NewVultrProvider wraps a Vultr client or fake.
func NewVultrProvider(api vultrAPI) *VultrProvider { return &VultrProvider{api: api} }

func fromVultr(r vultr.ReservedIP) Address {
	return Address{ID: r.ID, IP: r.Address, Label: r.Label, HolderID: r.InstanceID}
}

// mapVultr wraps a Vultr error around the engine's sentinel for it, keeping
// Vultr's own message for the log.
func mapVultr(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, vultr.ErrNotFound):
		return fmt.Errorf("%w: %w", ErrNotFound, err)
	case errors.Is(err, vultr.ErrQuotaExceeded):
		return fmt.Errorf("%w: %w", ErrQuotaExceeded, err)
	case errors.Is(err, vultr.ErrHolderBusy):
		return fmt.Errorf("%w: %w", ErrHolderBusy, err)
	}
	return err
}

func (p *VultrProvider) Create(ctx context.Context, label string) (Address, error) {
	r, err := p.api.CreateReservedIP(ctx, label)
	if err != nil {
		return Address{}, mapVultr(err)
	}
	return fromVultr(r), nil
}

func (p *VultrProvider) Attach(ctx context.Context, addrID, hostID string) error {
	return mapVultr(p.api.AttachReservedIP(ctx, addrID, hostID))
}

func (p *VultrProvider) Detach(ctx context.Context, addrID string) error {
	return mapVultr(p.api.DetachReservedIP(ctx, addrID))
}

func (p *VultrProvider) Delete(ctx context.Context, addrID string) error {
	return mapVultr(p.api.DeleteReservedIP(ctx, addrID))
}

func (p *VultrProvider) Get(ctx context.Context, addrID string) (Address, error) {
	r, err := p.api.GetReservedIP(ctx, addrID)
	if err != nil {
		return Address{}, mapVultr(err)
	}
	return fromVultr(r), nil
}

func (p *VultrProvider) List(ctx context.Context) ([]Address, error) {
	rs, err := p.api.ListReservedIPs(ctx)
	if err != nil {
		return nil, mapVultr(err)
	}
	out := make([]Address, 0, len(rs))
	for _, r := range rs {
		out = append(out, fromVultr(r))
	}
	return out, nil
}
