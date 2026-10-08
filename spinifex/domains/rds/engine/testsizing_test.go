package engine

//test:in-package — every test file in this package that needs a Sizing shares
// this fixture, built from the real EC2 instance-type table rather than a fake:
// the size-derived defaults are asserted against real memory footprints.

import (
	"github.com/mulgadc/spinifex/spinifex/domains/ec2/instancetypes"
)

type testInstanceTypes struct{}

func (testInstanceTypes) MemoryMiB(instanceType string) (int64, bool) {
	return instancetypes.DefaultMemoryMiB(instanceType)
}

var testSizing = NewSizing(testInstanceTypes{})
