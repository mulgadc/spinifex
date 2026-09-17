package instancetypes

import (
	"strings"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
)

// defaultMaxENIs is the conservative fallback ENI cap for unrecognized types.
const defaultMaxENIs = 4

// maxENIsByType overrides maxENIsByFamily for size-specific AWS ENI caps.
var maxENIsByType = map[string]int{
	"m5.large":   3,
	"m5.xlarge":  3,
	"m5.2xlarge": 4,
	"m5.4xlarge": 8,
}

// maxENIsByFamily is the per-family fallback when no type-specific entry exists.
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

// MaxENIsForType returns the total ENI cap (primary + secondaries) for an instance type.
// Precedence: explicit type → family → defaultMaxENIs.
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

// HotPlugENISlotsForType returns MaxENIsForType - 1 (the primary ENI occupies
// no hot-plug slot). Always ≥ 0.
func HotPlugENISlotsForType(instanceType string) int {
	n := MaxENIsForType(instanceType) - 1
	if n < 0 {
		return 0
	}
	return n
}

// NetworkInfoForType reports an instance type's ENI cap for DescribeInstanceTypes,
// from the same table the hot-plug slot allocator uses, so the advertised figure
// cannot drift from the enforced one. Fields this stack does not implement, such
// as secondary IPv4 addresses per interface, are left unset rather than guessed.
func NetworkInfoForType(instanceType string) *ec2.NetworkInfo {
	return &ec2.NetworkInfo{
		MaximumNetworkInterfaces: aws.Int64(int64(MaxENIsForType(instanceType))),
	}
}
