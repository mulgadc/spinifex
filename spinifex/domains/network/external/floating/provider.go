// Package floating implements external.Allocator for clouds whose public
// address is one object attached to one host, and moved by detaching it and
// attaching it elsewhere. Vultr reserved IPs are the first such provider.
//
// The engine owns the binding record, the create-then-record ordering, release,
// reconcile and the holder pass that puts each address on the node running its
// owner. A provider adapter owns only the API calls and their error mapping.
//
// Allocate never attaches. The node an address's owner runs on attaches it, so
// initial placement and a move after recovery are the same code path.
package floating

import (
	"context"
	"errors"
	"net/netip"
)

// Address is one provider address as the engine sees it.
type Address struct {
	ID    string
	IP    netip.Addr
	Label string
	// HolderID is the provider host the address is attached to; empty when
	// it is attached nowhere.
	HolderID string
}

// Provider is the vendor half of a floating public address. Errors are wrapped
// around the sentinels below so the engine can branch on them.
type Provider interface {
	// Create makes an unattached address carrying label.
	Create(ctx context.Context, label string) (Address, error)
	// Attach attaches addrID to hostID, succeeding if it is already there.
	Attach(ctx context.Context, addrID, hostID string) error
	// Detach detaches addrID from whichever host holds it, if any.
	Detach(ctx context.Context, addrID string) error
	// Delete releases addrID, detaching it first if the provider requires.
	Delete(ctx context.Context, addrID string) error
	// Get reads one address, including its holder.
	Get(ctx context.Context, addrID string) (Address, error)
	// List returns every address in the provider scope, ours or not.
	List(ctx context.Context) ([]Address, error)
}

var (
	// ErrNotFound means the address does not exist at the provider.
	ErrNotFound = errors.New("floating: address not found")
	// ErrQuotaExceeded means the provider will not create another address.
	ErrQuotaExceeded = errors.New("floating: provider address quota exceeded")
	// ErrHolderBusy means the address is attached to a different host.
	ErrHolderBusy = errors.New("floating: address attached to another host")
)
