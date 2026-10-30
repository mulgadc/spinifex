package gateway

import (
	"slices"

	"github.com/mulgadc/spinifex/spinifex/ingress/aws/dispatch"
)

// ServiceOperationInventory describes the authoritative dispatch state for an
// AWS-compatible service. Registered operations minus Stubbed and Unsupported
// are implemented handlers.
type ServiceOperationInventory struct {
	Registered  []string
	Stubbed     []string
	Unsupported []string
}

// AWSOperationInventory returns a fresh, stable snapshot of registered, the
// declared inventory of each registered service. S3 is intentionally absent:
// Spinifex does not dispatch S3 operations and delegates that REST surface to
// Predastore.
func AWSOperationInventory(registered map[string]dispatch.Inventory) map[string]ServiceOperationInventory {
	inventory := make(map[string]ServiceOperationInventory, len(registered))
	for name, inv := range registered {
		inventory[name] = ServiceOperationInventory{
			Registered:  slices.Clone(inv.Registered),
			Stubbed:     slices.Clone(inv.Stubbed),
			Unsupported: slices.Clone(inv.Unsupported),
		}
	}
	return inventory
}
