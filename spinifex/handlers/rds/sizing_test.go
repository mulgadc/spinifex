package handlers_rds

import (
	"testing"

	"github.com/mulgadc/spinifex/spinifex/awserrors"
	"github.com/mulgadc/spinifex/spinifex/instancetypes"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestInstanceTypeForClass_KnownClasses(t *testing.T) {
	tests := map[string]string{
		"db.t3.micro":  "t3.micro",
		"db.t3.small":  "t3.small",
		"db.t3.medium": "t3.medium",
		"db.t3.large":  "t3.large",
		"db.m5.large":  "m5.large",
		"db.m5.xlarge": "m5.xlarge",
	}
	for class, want := range tests {
		got, err := InstanceTypeForClass(class)
		require.NoError(t, err, "class %q", class)
		assert.Equal(t, want, got, "class %q", class)
	}
}

func TestInstanceTypeForClass_UnmappedClassRejected(t *testing.T) {
	// Real AWS classes the platform does not offer, the bare EC2 type, and a
	// class-shaped string that is not one — all must be rejected rather than
	// guessed at by stripping the db. prefix.
	for _, class := range []string{
		"db.r5.large", "db.m5.24xlarge", "db.t3.nano", "db.serverless",
		"m5.large", "", "db.", "DB.T3.MICRO",
	} {
		_, err := InstanceTypeForClass(class)
		require.Error(t, err, "class %q should be rejected", class)
		assert.Equal(t, awserrors.ErrorInvalidParameterValue, err.Error(), "class %q", class)
	}
}

// db.* is a facade over the platform's own sizing table, so every entry has to
// name an instance type that table actually defines. A typo here would surface
// as a launch failure after the data volume and ENI already exist.
func TestDBInstanceClasses_ResolveInSizingTable(t *testing.T) {
	for class, instanceType := range dbInstanceClasses {
		vcpus, ok := instancetypes.DefaultVCPUs(instanceType)
		assert.True(t, ok, "class %q maps to unknown instance type %q", class, instanceType)
		assert.Positive(t, vcpus, "instance type %q should have vCPUs", instanceType)
	}
}

func TestSupportedInstanceClasses_SortedAndComplete(t *testing.T) {
	assert.Equal(t, []string{
		"db.m5.large", "db.m5.xlarge",
		"db.t3.large", "db.t3.medium", "db.t3.micro", "db.t3.small",
	}, SupportedInstanceClasses())
}

// The literal matters: DescribeDBParameters reports its size-derived defaults at
// this class, and the alphabetically first class is db.m5.large — eight times the
// memory, so reading the head of the sorted list would advertise a shared_buffers
// no small instance ever runs.
func TestSmallestInstanceClass_IsTheLeastMemory(t *testing.T) {
	assert.Equal(t, "db.t3.micro", SmallestInstanceClass())

	least, err := classMemoryMiB(SmallestInstanceClass())
	require.NoError(t, err)
	for _, class := range SupportedInstanceClasses() {
		memoryMiB, err := classMemoryMiB(class)
		require.NoError(t, err, "every supported class needs a known footprint")
		assert.LessOrEqual(t, least, memoryMiB, "class %q is smaller than the one reported as smallest", class)
	}
}
