package instancetypes

import "strings"

// defaultMaxENIs is the fallback ENI cap for instance types not enumerated
// in maxENIsByType / maxENIsByFamily. Matches AWS's "small general purpose"
// envelope; deliberately conservative so undersized hosts cannot exhaust
// host taps from a single VM.
const defaultMaxENIs = 4

// maxENIsByType overrides maxENIsByFamily for size-specific caps where AWS
// docs publish a different number from the family default.
var maxENIsByType = map[string]int{
	"m5.large":   3,
	"m5.xlarge":  3,
	"m5.2xlarge": 4,
	"m5.4xlarge": 8,
}

// maxENIsByFamily is consulted when no type-specific entry exists. Sized to
// match AWS's published per-family ENI cap for representative members of
// each family; coarse on purpose for v1 — refine when a customer hits the
// cap. Keys are the dot-stripped family prefix (e.g. "t3", "m5").
var maxENIsByFamily = map[string]int{
	"t2":  3,
	"t3":  3,
	"t3a": 3,
	"t4g": 3,
	"m5":  15, // covers m5.8xlarge and larger; smaller sizes overridden above
	"m5a": 15,
	"m6i": 15,
	"m6a": 15,
	"m6g": 8,
	"m7i": 15,
	"m7a": 15,
	"m7g": 8,
	"m8i": 15,
	"m8a": 15,
	"m8g": 8,
}

// MaxENIsForType returns the total ENI cap for the given instance type
// (primary + secondaries). Lookup precedence: explicit type → family →
// defaultMaxENIs. The value is the hardware-imposed AWS limit, not a
// per-host quota; the host's hot-plug slot pool is sized as
// MaxENIsForType - 1 (the primary ENI consumes no hot-plug slot).
func MaxENIsForType(instanceType string) int {
	if n, ok := maxENIsByType[instanceType]; ok {
		return n
	}
	family, _, found := strings.Cut(instanceType, ".")
	if found {
		if n, ok := maxENIsByFamily[family]; ok {
			return n
		}
	}
	return defaultMaxENIs
}

// HotPlugENISlotsForType returns the number of PCIe root ports that must be
// pre-allocated at VM start for runtime ENI hot-plug. Equals
// MaxENIsForType - 1 since the primary ENI is wired at boot via the fixed
// netdev path and does not consume a hot-plug slot. Always ≥ 0.
func HotPlugENISlotsForType(instanceType string) int {
	n := MaxENIsForType(instanceType) - 1
	if n < 0 {
		return 0
	}
	return n
}
