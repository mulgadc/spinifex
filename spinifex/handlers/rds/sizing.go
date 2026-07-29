package handlers_rds

import (
	"errors"
	"maps"
	"slices"

	"github.com/mulgadc/spinifex/spinifex/awserrors"
	"github.com/mulgadc/spinifex/spinifex/instancetypes"
)

// db.* is a naming facade, not a second sizing table: every entry resolves
// through the platform's instance-type definitions.
var dbInstanceClasses = map[string]string{
	"db.t3.micro":  "t3.micro",
	"db.t3.small":  "t3.small",
	"db.t3.medium": "t3.medium",
	"db.t3.large":  "t3.large",
	"db.m5.large":  "m5.large",
	"db.m5.xlarge": "m5.xlarge",
}

// An unknown class is rejected here at validation rather than surfacing as a
// launch failure after the volume and ENI exist.
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

func SupportedInstanceClasses() []string {
	return slices.Sorted(maps.Keys(dbInstanceClasses))
}

// The supported class with the least memory. Deliberately computed from the
// footprints rather than taken as the first name SupportedInstanceClasses
// reports: that list is sorted alphabetically, so its head is db.m5.large.
func smallestInstanceClass() string {
	smallest, least := "", int64(0)
	for _, class := range SupportedInstanceClasses() {
		memoryMiB, err := classMemoryMiB(class)
		// A class with no known footprint cannot be compared; the catalog's
		// self-consistency test pins that every supported class has one.
		if err != nil {
			continue
		}
		if smallest == "" || memoryMiB < least {
			smallest, least = class, memoryMiB
		}
	}
	return smallest
}
