package handlers_rds

import (
	"errors"
	"maps"
	"slices"

	"github.com/mulgadc/spinifex/spinifex/awserrors"
	"github.com/mulgadc/spinifex/spinifex/instancetypes"
)

// dbInstanceClasses maps the curated db.* names RDS exposes onto the EC2
// instance type that actually sizes the VM (rds-v1.md D15). db.* is a naming
// facade for AWS compatibility, not a second sizing table: every entry resolves
// through the platform's vendor-agnostic instance-type definitions, so a class
// and its EC2 equivalent can never drift apart on vCPU or memory.
var dbInstanceClasses = map[string]string{
	"db.t3.micro":  "t3.micro",
	"db.t3.small":  "t3.small",
	"db.t3.medium": "t3.medium",
	"db.t3.large":  "t3.large",
	"db.m5.large":  "m5.large",
	"db.m5.xlarge": "m5.xlarge",
}

// InstanceTypeForClass resolves a db.* instance class to the EC2 instance type
// the DB VM launches as. A class outside the curated set, or one whose EC2
// equivalent the sizing table does not know, is rejected here at validation
// rather than surfacing as a launch failure after the volume and ENI exist.
func InstanceTypeForClass(class string) (string, error) {
	instanceType, ok := dbInstanceClasses[class]
	if !ok {
		return "", errors.New(awserrors.ErrorInvalidParameterValue)
	}
	if _, known := instancetypes.DefaultVCPUs(instanceType); !known {
		return "", errors.New(awserrors.ErrorInvalidParameterValue)
	}
	return instanceType, nil
}

// SupportedInstanceClasses returns the curated db.* class names in sorted order,
// for error messages and the orderable-options surface.
func SupportedInstanceClasses() []string {
	return slices.Sorted(maps.Keys(dbInstanceClasses))
}
