package floating

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"

	"github.com/mulgadc/spinifex/spinifex/providers/cloud/latitude"
)

// latitudeAPI is the slice of the latitude client the adapter calls, so tests can
// stand the fake in for it.
type latitudeAPI interface {
	GetElasticIP(ctx context.Context, id string) (latitude.ElasticIP, error)
	MoveElasticIP(ctx context.Context, id, serverID string) error
}

var (
	_ latitudeAPI = (*latitude.Client)(nil)
	_ latitudeAPI = (*latitude.Fake)(nil)
)

// LatitudeProvider adapts a fixed set of latitude.sh Elastic IPs to Provider.
// The set is named by ID, since the API has no labels to tell ours from the
// team's others. Addresses are never created, detached or deleted here: the API
// has no unassign, and the addresses belong to whoever bought them.
type LatitudeProvider struct {
	api latitudeAPI
	ids []string
}

var (
	_ Provider = (*LatitudeProvider)(nil)
	_ FixedSet = (*LatitudeProvider)(nil)
)

// NewLatitudeProvider wraps a latitude client or fake over the given Elastic IP IDs.
func NewLatitudeProvider(api latitudeAPI, ids []string) (*LatitudeProvider, error) {
	if len(ids) == 0 {
		return nil, errors.New("floating: latitude pool names no Elastic IP IDs")
	}
	return &LatitudeProvider{api: api, ids: slices.Clone(ids)}, nil
}

func fromLatitude(e latitude.ElasticIP) Address {
	return Address{ID: e.ID, IP: e.Address, HolderID: e.ServerID, Failed: e.Status == latitude.StatusError}
}

// mapLatitude wraps a latitude error around the engine's sentinel for it.
func mapLatitude(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, latitude.ErrNotFound):
		return fmt.Errorf("%w: %w", ErrNotFound, err)
	}
	return err
}

func (p *LatitudeProvider) member(id string) bool { return slices.Contains(p.ids, id) }

// Create is refused: the set is fixed.
func (p *LatitudeProvider) Create(context.Context, string) (Address, error) {
	return Address{}, errors.New("floating: a latitude pool cannot create addresses")
}

// Attach moves the address to hostID and returns once it is active there.
func (p *LatitudeProvider) Attach(ctx context.Context, addrID, hostID string) error {
	if !p.member(addrID) {
		return fmt.Errorf("%w: %s is not in this pool", ErrNotFound, addrID)
	}
	return mapLatitude(p.api.MoveElasticIP(ctx, addrID, hostID))
}

// Detach does nothing: latitude.sh cannot unassign an address, only move it.
func (p *LatitudeProvider) Detach(context.Context, string) error { return nil }

// Delete does nothing: the addresses are not ours to release.
func (p *LatitudeProvider) Delete(context.Context, string) error { return nil }

func (p *LatitudeProvider) Get(ctx context.Context, addrID string) (Address, error) {
	if !p.member(addrID) {
		return Address{}, fmt.Errorf("%w: %s is not in this pool", ErrNotFound, addrID)
	}
	e, err := p.api.GetElasticIP(ctx, addrID)
	if err != nil {
		return Address{}, mapLatitude(err)
	}
	return fromLatitude(e), nil
}

// List returns the pool's members.
func (p *LatitudeProvider) List(ctx context.Context) ([]Address, error) {
	return p.Members(ctx)
}

// Members reads each configured address by ID. The project listing is not used: it
// ignores pagination and is refused when a page is full, so it cannot say who is in the set.
// An address latitude.sh no longer has is logged and left out, so the rest of the set keeps
// working; any other failure is returned, since the call did not establish who is there.
func (p *LatitudeProvider) Members(ctx context.Context) ([]Address, error) {
	out := make([]Address, 0, len(p.ids))
	for _, id := range p.ids {
		e, err := p.api.GetElasticIP(ctx, id)
		if errors.Is(err, latitude.ErrNotFound) {
			slog.ErrorContext(ctx, "floating: a configured Elastic IP no longer exists at latitude.sh; the pool runs without it",
				"pool_member", id)
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("floating: configured Elastic IP %s: %w", id, mapLatitude(err))
		}
		if why := e.Unusable(); why != "" {
			return nil, fmt.Errorf("floating: configured Elastic IP %s is unusable: %s", id, why)
		}
		out = append(out, fromLatitude(e))
	}
	return out, nil
}

// reportMembers reads each configured address once at start and logs, naming the pool,
// each one the allocator will not use and a pool left with none. It returns a read
// failure other than not found if one occurs, else the first unusable member's refusal.
func (p *LatitudeProvider) reportMembers(ctx context.Context, pool string) error {
	usable := 0
	var refused error
	for _, id := range p.ids {
		e, err := p.api.GetElasticIP(ctx, id)
		switch {
		case errors.Is(err, latitude.ErrNotFound):
			slog.ErrorContext(ctx, "floating: a configured Elastic IP will not be used: it does not exist at latitude.sh",
				"pool", pool, "pool_member", id)
		case err != nil:
			return fmt.Errorf("floating: configured Elastic IP %s: %w", id, mapLatitude(err))
		case e.Unusable() != "":
			slog.ErrorContext(ctx, "floating: a configured Elastic IP will not be used: the pool refuses all its addresses while it is configured",
				"pool", pool, "pool_member", id, "reason", e.Unusable())
			if refused == nil {
				refused = fmt.Errorf("floating: configured Elastic IP %s is unusable: %s", id, e.Unusable())
				logPoolUnusable(ctx, pool)
			}
		case e.Status == latitude.StatusError:
			slog.ErrorContext(ctx, "floating: a configured Elastic IP will not be used: latitude.sh reports it in the error state",
				"pool", pool, "pool_member", id)
		default:
			usable++
		}
	}
	if usable == 0 && refused == nil {
		logPoolUnusable(ctx, pool)
	}
	return refused
}

// logPoolUnusable is logged as soon as the pool is known to be unusable, so a later
// read failure cannot hide it.
func logPoolUnusable(ctx context.Context, pool string) {
	slog.ErrorContext(ctx, "floating: pool has no usable address; public addresses will fail until fixed", "pool", pool)
}
