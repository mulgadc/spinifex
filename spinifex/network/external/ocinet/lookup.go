package ocinet

import (
	"context"
	"errors"
	"fmt"
	"net/netip"

	"github.com/mulgadc/spinifex/spinifex/kvstore"
)

// Lookup answers "what address does this external IP ride the wire as" from the
// bindings store alone. No OCI client and no credentials, because the process
// that needs the answer is vpcd: it programs the OVN NAT rule and the host
// ingress route, and both must match the private half of the pair while every
// AWS-facing record holds the public half.
//
// Reading KV rather than asking the allocator is not duplication of state —
// vpcd is a separate process from the daemon that owns the allocator, and the
// bindings record is the only thing either of them would consult.
type Lookup struct {
	store Store
	pools []string
}

// NewLookup binds a read-only resolver to the named OCI pools.
func NewLookup(store Store, pools []string) *Lookup {
	return &Lookup{store: store, pools: append([]string(nil), pools...)}
}

// DatapathIP maps externalIP to its on-wire private address, or returns it
// unchanged when no OCI pool holds a binding for it — every address on a
// static or DHCP pool is its own datapath address, and vpcd cannot tell which
// pool an address came from without asking.
//
// A store read failure is an error rather than a pass-through. Falling back to
// the public address would install a NAT rule matching an address that never
// appears on the wire, which is indistinguishable from a working rule until
// the instance turns out to be unreachable.
func (l *Lookup) DatapathIP(ctx context.Context, externalIP string) (string, error) {
	if l == nil || externalIP == "" {
		return externalIP, nil
	}
	for _, pool := range l.pools {
		rec, err := l.store.Get(ctx, pool)
		if errors.Is(err, kvstore.ErrNotFound) {
			// A pool that has never allocated holds no bindings, which is a
			// miss on this pool rather than a reason to fail the lookup.
			continue
		}
		if err != nil {
			return "", fmt.Errorf("ocinet: read bindings for pool %q: %w", pool, err)
		}
		b, ok := rec.Bindings[externalIP]
		if !ok {
			continue
		}
		if _, err := netip.ParseAddr(b.PrivateAddr); err != nil {
			return "", fmt.Errorf("ocinet: binding for %s in pool %q has unparseable private addr %q: %w",
				externalIP, pool, b.PrivateAddr, err)
		}
		return b.PrivateAddr, nil
	}
	return externalIP, nil
}
