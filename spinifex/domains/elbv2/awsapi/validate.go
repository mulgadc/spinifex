package awsapi

import (
	"fmt"
	"regexp"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
)

// healthCheckProtocols is the model's ProtocolEnum, which HealthCheckProtocol shares.
var healthCheckProtocols = []string{"HTTP", "HTTPS", "TCP", "TLS", "UDP", "TCP_UDP", "GENEVE", "QUIC", "TCP_QUIC"}

// ipAddressTypes is the model's IpAddressType enum.
var ipAddressTypes = []string{"ipv4", "dualstack", "dualstack-without-public-ipv4"}

// loadBalancerTypes is the model's LoadBalancerTypeEnum.
var loadBalancerTypes = []string{"application", "network", "gateway"}

var customerOwnedIpv4PoolPattern = regexp.MustCompile(`^(ipv4pool-coip-)[a-zA-Z0-9]+$`)

// constraintError renders a model constraint break the way AWS words it.
func constraintError(value any, member, constraint string) error {
	return awserrors.Errorf(awserrors.ErrorValidationError,
		"1 validation error detected: Value '%v' at '%s' failed to satisfy constraint: %s", value, member, constraint)
}

// checkRange refuses a set integer outside [lo, hi].
func checkRange(v *int64, member string, lo, hi int64) error {
	switch {
	case v == nil:
		return nil
	case *v < lo:
		return constraintError(*v, member, fmt.Sprintf("Member must have value greater than or equal to %d", lo))
	case *v > hi:
		return constraintError(*v, member, fmt.Sprintf("Member must have value less than or equal to %d", hi))
	}
	return nil
}

// checkEnum refuses a set, non-empty string outside allowed.
func checkEnum(v *string, member string, allowed []string) error {
	if v == nil || *v == "" || slices.Contains(allowed, *v) {
		return nil
	}
	return constraintError(*v, member, "Member must satisfy enum value set: ["+strings.Join(allowed, ", ")+"]")
}

// checkLengthPattern refuses a set, non-empty string longer than maxLen characters or not matching re.
func checkLengthPattern(v *string, member string, maxLen int, re *regexp.Regexp) error {
	if v == nil || *v == "" {
		return nil
	}
	if utf8.RuneCountInString(*v) > maxLen {
		return constraintError(*v, member, fmt.Sprintf("Member must have length less than or equal to %d", maxLen))
	}
	if !re.MatchString(*v) {
		return constraintError(*v, member, "Member must satisfy regular expression pattern: "+re.String())
	}
	return nil
}

// validateHealthCheck applies the model's health check enum and ranges shared by
// CreateTargetGroup and ModifyTargetGroup.
func validateHealthCheck(protocol *string, interval, timeout, healthy, unhealthy *int64) error {
	if err := checkEnum(protocol, "healthCheckProtocol", healthCheckProtocols); err != nil {
		return err
	}
	if err := checkRange(interval, "healthCheckIntervalSeconds", 5, 300); err != nil {
		return err
	}
	if err := checkRange(timeout, "healthCheckTimeoutSeconds", 2, 120); err != nil {
		return err
	}
	if err := checkRange(healthy, "healthyThresholdCount", 2, 10); err != nil {
		return err
	}
	return checkRange(unhealthy, "unhealthyThresholdCount", 2, 10)
}
