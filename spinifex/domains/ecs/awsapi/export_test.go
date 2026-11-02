package awsapi

// Unexported hooks the external test package needs. The SDK-gap helpers are
// deliberately unexported on the real surface: nothing outside this package
// may add response fields the daemon did not supply.
const (
	AvailabilityZoneRebalancingField = availabilityZoneRebalancingField
	AZRebalancingDisabled            = azRebalancingDisabled
)

var (
	CheckAZRebalancing = checkAZRebalancing
	AddSDKGapFields    = addSDKGapFields
)
