package engine

import (
	"errors"
	"maps"
	"slices"
	"strings"

	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
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

// InstanceTypes is the narrow EC2 fact this package needs: an instance type's
// memory footprint. The production implementation, backed by domains/ec2/
// instancetypes, is composed outside this package by whoever builds a Sizing.
type InstanceTypes interface {
	MemoryMiB(instanceType string) (memoryMiB int64, ok bool)
}

// Sizing is the class→memory projection every size-derived parameter default is
// computed from. It is built once, at composition time: no live availability,
// no cluster state, no NATS.
type Sizing struct {
	memoryMiB map[string]int64
}

// NewSizing precomputes the db.* class to memory projection from types. A class
// whose backing EC2 type types does not know is excluded rather than failing
// construction, so one unsupported class does not take the whole catalog down;
// InstanceTypeForClass reports the same exclusion at validation. types is a
// composition-time dependency; a nil one is a wiring bug that fails on first
// use here, not a condition this constructor reports.
func NewSizing(types InstanceTypes) Sizing {
	memoryMiB := make(map[string]int64, len(dbInstanceClasses))
	for class, instanceType := range dbInstanceClasses {
		if mem, ok := types.MemoryMiB(instanceType); ok && mem > 0 {
			memoryMiB[class] = mem
		}
	}
	return Sizing{memoryMiB: memoryMiB}
}

// InstanceTypeForClass maps a db.* instance class to the EC2 instance type backing it. An unknown class
// is rejected here at validation rather than surfacing as a launch failure after the volume and ENI exist.
func (s Sizing) InstanceTypeForClass(class string) (string, error) {
	instanceType, ok := dbInstanceClasses[class]
	if !ok {
		return "", errors.New(awserrors.ErrorInvalidParameterValue)
	}
	if _, known := s.memoryMiB[class]; !known {
		return "", errors.New(awserrors.ErrorInvalidParameterValue)
	}
	return instanceType, nil
}

// SupportedInstanceClasses returns every db.* instance class this platform offers, sorted alphabetically.
// Unlike InstanceTypeForClass this does not depend on Sizing: the name facade is static regardless of
// which classes a particular instance-types source can resolve.
func SupportedInstanceClasses() []string {
	return slices.Sorted(maps.Keys(dbInstanceClasses))
}

// SmallestInstanceClass returns the supported class with the least memory. Deliberately computed from
// the footprints rather than taken as the first name SupportedInstanceClasses reports: that list is
// sorted alphabetically, so its head is db.m5.large.
func (s Sizing) SmallestInstanceClass() string {
	smallest, least := "", int64(0)
	for _, class := range SupportedInstanceClasses() {
		memoryMiB, err := s.ClassMemoryMiB(class)
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

// ClassMemoryMiB returns the memory an instance class's guest has, which every
// size-derived default is computed from. An unknown class is a validation
// failure upstream, so this is the last line rather than the check.
func (s Sizing) ClassMemoryMiB(instanceClass string) (int64, error) {
	instanceType, err := s.InstanceTypeForClass(instanceClass)
	if err != nil {
		return 0, awserrors.Errorf(awserrors.ErrorInvalidParameterValue,
			"DBInstanceClass %q is not supported; supported classes are %s", instanceClass, strings.Join(SupportedInstanceClasses(), ", "))
	}
	memoryMiB, ok := s.memoryMiB[instanceClass]
	if !ok || memoryMiB <= 0 {
		return 0, awserrors.Errorf(awserrors.ErrorServerInternal,
			"no memory footprint is known for instance type %s", instanceType)
	}
	return memoryMiB, nil
}
