package engine

import (
	"testing"

	"github.com/mulgadc/spinifex/spinifex/domains/ec2/instancetypes"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestInstanceTypeForClass_KnownClasses(t *testing.T) {
	t.Parallel()
	tests := map[string]string{
		"db.t3.micro":  "t3.micro",
		"db.t3.small":  "t3.small",
		"db.t3.medium": "t3.medium",
		"db.t3.large":  "t3.large",
		"db.m5.large":  "m5.large",
		"db.m5.xlarge": "m5.xlarge",
	}
	for class, want := range tests {
		got, err := testSizing.InstanceTypeForClass(class)
		require.NoError(t, err, "class %q", class)
		assert.Equal(t, want, got, "class %q", class)
	}
}

func TestInstanceTypeForClass_UnmappedClassRejected(t *testing.T) {
	t.Parallel()
	// Real AWS classes the platform does not offer, the bare EC2 type, and a
	// class-shaped string that is not one — all must be rejected rather than
	// guessed at by stripping the db. prefix.
	for _, class := range []string{
		"db.r5.large", "db.m5.24xlarge", "db.t3.nano", "db.serverless",
		"m5.large", "", "db.", "DB.T3.MICRO",
	} {
		_, err := testSizing.InstanceTypeForClass(class)
		require.Error(t, err, "class %q should be rejected", class)
		assert.Equal(t, awserrors.ErrorInvalidParameterValue, err.Error(), "class %q", class)
	}
}

// db.* is a facade over the platform's own sizing table, so every entry has to
// name an instance type that table actually defines. A typo here would surface
// as a launch failure after the data volume and ENI already exist.
func TestDBInstanceClasses_ResolveInSizingTable(t *testing.T) {
	t.Parallel()
	for class, instanceType := range dbInstanceClasses {
		vcpus, ok := instancetypes.DefaultVCPUs(instanceType)
		assert.True(t, ok, "class %q maps to unknown instance type %q", class, instanceType)
		assert.Positive(t, vcpus, "instance type %q should have vCPUs", instanceType)
	}
}

func TestSupportedInstanceClasses_SortedAndComplete(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
	assert.Equal(t, "db.t3.micro", testSizing.SmallestInstanceClass())

	least, err := testSizing.ClassMemoryMiB(testSizing.SmallestInstanceClass())
	require.NoError(t, err)
	for _, class := range SupportedInstanceClasses() {
		memoryMiB, err := testSizing.ClassMemoryMiB(class)
		require.NoError(t, err, "every supported class needs a known footprint")
		assert.LessOrEqual(t, least, memoryMiB, "class %q is smaller than the one reported as smallest", class)
	}
}

// The literal DescribeDBParameters' size-derived defaults resolve against for
// the smallest class: db.t3.micro's 1 GiB, stated here as the MiB ClassMemoryMiB
// actually returns rather than only the ordering checked above.
func TestSmallestInstanceClass_MemoryIsExactlyOneGiB(t *testing.T) {
	t.Parallel()
	memoryMiB, err := testSizing.ClassMemoryMiB(testSizing.SmallestInstanceClass())
	require.NoError(t, err)
	assert.Equal(t, int64(1024), memoryMiB)
}
