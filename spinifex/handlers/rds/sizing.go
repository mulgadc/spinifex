package handlers_rds

import (
	"github.com/mulgadc/spinifex/spinifex/domains/ec2/instancetypes"
	rdsengine "github.com/mulgadc/spinifex/spinifex/domains/rds/engine"
)

// ec2InstanceTypes adapts the platform's instance-type table to the engine
// package's narrow InstanceTypes seam, keeping that package free of an EC2
// import.
type ec2InstanceTypes struct{}

var _ rdsengine.InstanceTypes = ec2InstanceTypes{}

func (ec2InstanceTypes) MemoryMiB(instanceType string) (int64, bool) {
	return instancetypes.DefaultMemoryMiB(instanceType)
}

// InstanceSizing builds the class-to-memory projection every size-derived
// parameter default is computed from, backed by this platform's own
// instance-type table.
func InstanceSizing() rdsengine.Sizing {
	return rdsengine.NewSizing(ec2InstanceTypes{})
}
