package dns

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestEC2Names(t *testing.T) {
	assert.Equal(t, "ec2-1-2-3-4.ap-southeast-2.compute.spx3.net",
		EC2PublicName("1.2.3.4", "ap-southeast-2", "spx3.net"))
	assert.Equal(t, "ip-172-31-26-216.ap-southeast-2.compute.internal",
		EC2PrivateName("172.31.26.216", "ap-southeast-2", ""))
	// A configured internal domain overrides the compute.internal default.
	assert.Equal(t, "ip-172-31-26-216.ap-southeast-2.internal.example",
		EC2PrivateName("172.31.26.216", "ap-southeast-2", "internal.example"))
}

func TestEC2Changes(t *testing.T) {
	// Public + private both present.
	changes := EC2Changes(ActionUpsert, "ap-southeast-2", "spx3.net", "", "1.2.3.4", "172.31.26.216")
	require.Len(t, changes, 2)
	assert.Equal(t, "spx3.net", changes[0].Zone)
	assert.Equal(t, "1.2.3.4", changes[0].Value)
	assert.Equal(t, PrivateZone, changes[1].Zone)
	assert.Equal(t, "172.31.26.216", changes[1].Value)

	// No public IP → only the private record.
	changes = EC2Changes(ActionUpsert, "ap-southeast-2", "spx3.net", "", "", "172.31.26.216")
	require.Len(t, changes, 1)
	assert.Equal(t, PrivateZone, changes[0].Zone)

	// A configured internal domain is used for the private zone + name.
	changes = EC2Changes(ActionUpsert, "ap-southeast-2", "spx3.net", "internal.example", "", "172.31.26.216")
	require.Len(t, changes, 1)
	assert.Equal(t, "internal.example", changes[0].Zone)
	assert.Equal(t, "ip-172-31-26-216.ap-southeast-2.internal.example", changes[0].Name)

	// No region → nothing.
	assert.Empty(t, EC2Changes(ActionUpsert, "", "spx3.net", "", "1.2.3.4", "172.31.26.216"))
}

func TestRelativeLabel(t *testing.T) {
	assert.Equal(t, "", relativeLabel("spx3.net", "spx3.net"))
	assert.Equal(t, "", relativeLabel("spx3.net.", "spx3.net"))
	assert.Equal(t, "ec2-1-2-3-4.ap-southeast-2.compute.",
		relativeLabel("ec2-1-2-3-4.ap-southeast-2.compute.spx3.net", "spx3.net"))
	assert.Equal(t, "ip-10-0-0-1.ap-southeast-2.",
		relativeLabel("ip-10-0-0-1.ap-southeast-2.compute.internal", "compute.internal"))
}
