package floating

import (
	"context"
	"errors"
	"fmt"

	"github.com/mulgadc/spinifex/spinifex/foundation/state/kvstore"
)

// GatewayBinder records which VPC's NAT gateway answers on a floating address.
// vpcd calls it from the reconcile that decides the gateway chassis, since a
// NAT gateway's address has no ENI to place it by.
type GatewayBinder struct {
	store Store
	pools []string
}

// NewGatewayBinder covers the named floating pools.
func NewGatewayBinder(store Store, pools []string) *GatewayBinder {
	return &GatewayBinder{store: store, pools: pools}
}

// BindGateway records vpcID against externalIP in whichever pool holds it.
// Idempotent: an unchanged VPC is not a write.
func (g *GatewayBinder) BindGateway(ctx context.Context, externalIP, vpcID string) error {
	if g == nil || externalIP == "" {
		return nil
	}
	for _, pool := range g.pools {
		err := g.store.Mutate(ctx, pool, func(rec *Record) (bool, error) {
			b, ok := rec.Bindings[externalIP]
			if !ok || b.GatewayVPCID == vpcID {
				return false, nil
			}
			b.GatewayVPCID = vpcID
			rec.Bindings[externalIP] = b
			return true, nil
		})
		if errors.Is(err, kvstore.ErrNotFound) {
			continue
		}
		if err != nil {
			return fmt.Errorf("floating: record gateway VPC for %s in pool %q: %w", externalIP, pool, err)
		}
	}
	return nil
}
