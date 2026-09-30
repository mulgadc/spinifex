package handlers_ec2_vpc

import (
	"strings"

	"github.com/mulgadc/spinifex/spinifex/awserrors"
)

// isLowerHex reports whether s is non-empty and holds only lowercase hex digits.
func isLowerHex(s string) bool {
	if s == "" {
		return false
	}
	for _, c := range s {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// malformedIDError is AWS's Invalid id answer for an ID that is not of the
// form <prefix>-<suffix>, or whose suffix fails suffixOK.
func malformedIDError(code, prefix, id string, suffixOK func(string) bool) error {
	suffix, ok := strings.CutPrefix(id, prefix+"-")
	if !ok {
		return awserrors.Errorf(code, "Invalid id: %q (expecting %q)", id, prefix+"-...")
	}
	if !suffixOK(suffix) {
		return awserrors.Errorf(code, "Invalid id: %q", id)
	}
	return nil
}

// sgIDMalformedError returns AWS's error for a group ID it calls malformed, or
// nil. AWS answers NotFound for up to 8 hex digits or exactly 17, the length
// of every Spinifex ID; other lengths are Malformed.
func sgIDMalformedError(id string) error {
	return malformedIDError(awserrors.ErrorInvalidGroupIdMalformed, "sg", id, func(s string) bool {
		return isLowerHex(s) && (len(s) <= 8 || len(s) == 17)
	})
}

// eniIDMalformedError returns AWS's error for an interface ID it calls
// malformed, or nil. AWS answers NotFound for lowercase hex of any length.
func eniIDMalformedError(id string) error {
	return malformedIDError(awserrors.ErrorInvalidNetworkInterfaceIdMalformed, "eni", id, isLowerHex)
}

func vpcNotFoundError(id string) error {
	return awserrors.IDNotFound(awserrors.ErrorInvalidVpcIDNotFound, "vpc", id)
}

func eniNotFoundError(id string) error {
	return awserrors.IDNotFound(awserrors.ErrorInvalidNetworkInterfaceIDNotFound, "networkInterface", id)
}

// sgNotFoundError is AWS's security group not-found answer, which unlike the
// other resource types says no "ID".
func sgNotFoundError(id string) error {
	return awserrors.Errorf(awserrors.ErrorInvalidGroupNotFound, "The security group '%s' does not exist", id)
}

// subnetRangeError answers a subnet CIDR too large, too small or outside
// its VPC, which AWS does not tell apart.
func subnetRangeError(cidr string) error {
	return awserrors.Errorf(awserrors.ErrorInvalidSubnetRange, "The CIDR '%s' is invalid.", cidr)
}
