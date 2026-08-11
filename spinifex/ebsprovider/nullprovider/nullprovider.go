// Package nullprovider serves the EBSProvider contract without any storage
// behind it. It exists to establish the floor: a measurement taken against it
// is the cost of the contract, the transport and the control plane's own
// bookkeeping, with no engine, no object store and no disk in the number.
//
// It is not a type of its own. ebsprovider.MemoryProvider already answers
// every verb from a map, and natsserve already serves any EBSProvider, so a
// null provider is those two composed; this package only names that
// composition and states what it does and does not measure.
package nullprovider

import (
	"context"
	"fmt"

	"github.com/mulgadc/spinifex/spinifex/ebsprovider"
	"github.com/mulgadc/spinifex/spinifex/ebsprovider/natsserve"
	"github.com/nats-io/nats.go"
)

// Capabilities is what a null provider advertises. Everything the contract
// makes optional is on: the point is to price the verbs, and a capability
// turned off is a verb that returns early instead of being measured.
var Capabilities = ebsprovider.Capabilities{
	CopyOnWriteClone:        true,
	OnlineExpansion:         true,
	SparseExtentReporting:   true,
	CrashConsistentSnapshot: true,
	VolumeSeeding:           true,
	ReadOnlyPublish:         true,
	VolumeEnumeration:       true,
}

// New returns an in-process null provider. Calls against it skip NATS
// entirely, so it prices the contract's own validation and bookkeeping and
// nothing else.
func New() *ebsprovider.MemoryProvider {
	return ebsprovider.NewMemoryProvider(Capabilities)
}

// Serve stands a null provider up on nc and returns a client that reaches it
// the way the control plane reaches any provider: encode, publish, queue
// dispatch, decode. The difference between this and New is the transport, and
// the difference between this and a real provider is the storage.
//
// PublishVolume answers with a URI nothing serves, so this bounds control
// operations only. It cannot stand in for a data-path comparison; qemunbdd is
// the control for that.
func Serve(ctx context.Context, nc *nats.Conn, opts natsserve.Options) (client *ebsprovider.NATSProvider, stop func(), err error) {
	stop, err = natsserve.Serve(ctx, nc, New(), opts)
	if err != nil {
		return nil, nil, fmt.Errorf("serve null provider: %w", err)
	}
	return ebsprovider.NewNATSProvider(nc, 0), stop, nil
}
