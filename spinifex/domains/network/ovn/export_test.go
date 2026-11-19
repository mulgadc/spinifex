package ovn

import (
	"context"

	"github.com/mulgadc/spinifex/spinifex/domains/network/ovn/nbdb"
)

// SetAddressSetInsertHook installs f between CreateAddressSetIfAbsent's cache
// miss and its insert until the test ends.
func SetAddressSetInsertHook(t interface{ Cleanup(func()) }, f func()) {
	addressSetInsertHook = f
	t.Cleanup(func() { addressSetInsertHook = nil })
}

// DeleteAddressSet removes the named set, for tests that recreate one.
func DeleteAddressSet(ctx context.Context, t interface{ Fatalf(string, ...any) }, c *LiveClient, name string) {
	ops, err := c.client.Where(&nbdb.AddressSet{Name: name}).Delete()
	if err == nil {
		err = c.transactOps(ctx, ops)
	}
	if err != nil {
		t.Fatalf("delete address set %s: %v", name, err)
	}
}
