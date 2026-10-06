package ocinet_test

import (
	"context"
	"errors"
	"net/netip"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mulgadc/spinifex/spinifex/cloud/oci"
)

// The primary private IP is what carries the subnet OCID, so the check needs one
// seeded to have anything to ask about.
func seedPrimary(fake *oci.Fake) {
	fake.SeedPrivateIP(oci.PrivateIP{
		ID:        "ocid1.privateip.oc1..primary",
		Address:   netip.MustParseAddr("10.200.0.2"),
		VNICID:    "ocid1.vnic.oc1..vnic1",
		SubnetID:  "ocid1.subnet.oc1..subnet1",
		IsPrimary: true,
	})
}

func TestCheckAuthorisationPassesWhenEveryPermissionIsHeld(t *testing.T) {
	fake := oci.NewFake()
	seedPrimary(fake)
	a, _ := newTestAllocator(t, fake)

	require.NoError(t, a.CheckAuthorisation(context.Background()))
}

// The defect this guards: a credential granted vnics and private-ips but no
// subnet verb reads both of those fine and is refused every AssignPrivateIP.
func TestCheckAuthorisationFailsWhenTheSubnetVerbIsMissing(t *testing.T) {
	fake := oci.NewFake()
	seedPrimary(fake)
	fake.FailWith["GetSubnet"] = errors.New("NotAuthorizedOrNotFound")
	a, _ := newTestAllocator(t, fake)

	err := a.CheckAuthorisation(context.Background())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "use subnets")
	assert.Contains(t, err.Error(), "SUBNET_ATTACH")
}

func TestCheckAuthorisationFailsWhenTheVNICCannotBeRead(t *testing.T) {
	fake := oci.NewFake()
	seedPrimary(fake)
	fake.FailWith["ListPrivateIPs"] = errors.New("NotAuthorizedOrNotFound")
	a, _ := newTestAllocator(t, fake)

	err := a.CheckAuthorisation(context.Background())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "use vnics")
}

// An empty list means the OCID is not a VNIC this credential can see, which is a
// different fault from a missing verb and has to read differently.
func TestCheckAuthorisationFailsWhenTheVNICHasNoSubnet(t *testing.T) {
	a, _ := newTestAllocator(t, oci.NewFake())

	err := a.CheckAuthorisation(context.Background())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no private IP with a subnet")
}
