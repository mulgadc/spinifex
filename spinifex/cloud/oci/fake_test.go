package oci_test

import (
	"context"
	"errors"
	"net/netip"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mulgadc/spinifex/spinifex/cloud/oci"
)

// The Fake is what every allocator test is measured against, so a Fake that
// models OCI wrongly makes those tests agree on a fiction. These check the
// behaviours the allocator actually depends on.

func TestFakeAssignsSuccessiveAddressesLikeOCIChoosingThem(t *testing.T) {
	ctx := context.Background()
	f := oci.NewFake()

	// The caller passes the zero Addr: OCI picks, not us. That asymmetry with
	// a static pool is the whole reason this package exists.
	a, err := f.AssignPrivateIP(ctx, "vnic1", netip.Addr{}, "spinifex-1")
	require.NoError(t, err)
	b, err := f.AssignPrivateIP(ctx, "vnic1", netip.Addr{}, "spinifex-2")
	require.NoError(t, err)

	assert.Equal(t, "10.200.0.100", a.Address.String())
	assert.Equal(t, "10.200.0.101", b.Address.String())
	assert.NotEqual(t, a.ID, b.ID, "each assignment is a distinct OCI object")
	assert.Equal(t, "vnic1", a.VNICID)
}

func TestFakeHonoursACallerChosenAddress(t *testing.T) {
	ctx := context.Background()
	f := oci.NewFake()

	got, err := f.AssignPrivateIP(ctx, "vnic1", netip.MustParseAddr("10.200.0.183"), "spinifex-1")
	require.NoError(t, err)
	assert.Equal(t, "10.200.0.183", got.Address.String())
}

// OCI caps private IPs at 64 per VNIC and the allocator has to surface that as
// InsufficientAddressCapacity, so the Fake must refuse with the sentinel rather
// than with a generic error.
func TestFakeRefusesPastThePrivateIPLimit(t *testing.T) {
	ctx := context.Background()
	f := oci.NewFake()
	f.PrivateLimit = 1

	_, err := f.AssignPrivateIP(ctx, "vnic1", netip.Addr{}, "spinifex-1")
	require.NoError(t, err)

	_, err = f.AssignPrivateIP(ctx, "vnic1", netip.Addr{}, "spinifex-2")
	require.Error(t, err)
	assert.ErrorIs(t, err, oci.ErrLimitExceeded)
}

// A create returns ASSIGNING and the address is not reachable until it settles.
// An allocator that returned on the first response would hand out a dead
// address, which is why the poll ladder exists.
func TestFakeModelsAsynchronousAssignment(t *testing.T) {
	ctx := context.Background()
	f := oci.NewFake()
	f.AssignAfter = 2

	priv, err := f.AssignPrivateIP(ctx, "vnic1", netip.Addr{}, "spinifex-1")
	require.NoError(t, err)
	pub, err := f.CreatePublicIP(ctx, "comp1", priv.ID, "spinifex-1")
	require.NoError(t, err)
	assert.False(t, pub.IsAssigned(), "a fresh public IP must not claim to be assigned")

	first, err := f.GetPublicIP(ctx, pub.ID)
	require.NoError(t, err)
	assert.False(t, first.IsAssigned())

	second, err := f.GetPublicIP(ctx, pub.ID)
	require.NoError(t, err)
	assert.True(t, second.IsAssigned(), "it must settle after the configured number of polls")
}

func TestFakeCreatesReservedPublicIPsAttachedToTheirPrivateIP(t *testing.T) {
	ctx := context.Background()
	f := oci.NewFake()

	priv, err := f.AssignPrivateIP(ctx, "vnic1", netip.Addr{}, "spinifex-1")
	require.NoError(t, err)
	pub, err := f.CreatePublicIP(ctx, "comp1", priv.ID, "spinifex-1")
	require.NoError(t, err)

	assert.Equal(t, oci.LifetimeReserved, pub.Lifetime,
		"an EIP must outlive its association, which is what RESERVED means")
	assert.Equal(t, priv.ID, pub.PrivateIPID)
	assert.Equal(t, "203.0.113.10", pub.Address.String())

	found, err := f.GetPublicIPByPrivateIPID(ctx, priv.ID)
	require.NoError(t, err)
	assert.Equal(t, pub.ID, found.ID)
}

func TestFakeReportsNotFoundForObjectsThatAreGone(t *testing.T) {
	ctx := context.Background()
	f := oci.NewFake()

	_, err := f.GetPrivateIP(ctx, "ocid1.privateip.oc1..missing")
	assert.ErrorIs(t, err, oci.ErrNotFound)

	_, err = f.GetPublicIP(ctx, "ocid1.publicip.oc1..missing")
	assert.ErrorIs(t, err, oci.ErrNotFound)

	_, err = f.GetPublicIPByPrivateIPID(ctx, "ocid1.privateip.oc1..missing")
	assert.ErrorIs(t, err, oci.ErrNotFound,
		"reconcile distinguishes a leaked private IP with no public IP from an API failure by this")
}

func TestFakeDetachLeavesThePublicIPButUnassigned(t *testing.T) {
	ctx := context.Background()
	f := oci.NewFake()

	priv, err := f.AssignPrivateIP(ctx, "vnic1", netip.Addr{}, "spinifex-1")
	require.NoError(t, err)
	pub, err := f.CreatePublicIP(ctx, "comp1", priv.ID, "spinifex-1")
	require.NoError(t, err)

	got, err := f.DetachPublicIP(ctx, pub.ID)
	require.NoError(t, err)
	assert.Empty(t, got.PrivateIPID)
	assert.False(t, got.IsAssigned())
	assert.Len(t, f.PublicIPs(), 1, "detach unassigns; only delete removes the object and stops the bill")
}

func TestFakeAttachRepointsAPublicIP(t *testing.T) {
	ctx := context.Background()
	f := oci.NewFake()

	first, err := f.AssignPrivateIP(ctx, "vnic1", netip.Addr{}, "spinifex-1")
	require.NoError(t, err)
	second, err := f.AssignPrivateIP(ctx, "vnic1", netip.Addr{}, "spinifex-2")
	require.NoError(t, err)
	pub, err := f.CreatePublicIP(ctx, "comp1", first.ID, "spinifex-1")
	require.NoError(t, err)

	got, err := f.AttachPublicIP(ctx, pub.ID, second.ID)
	require.NoError(t, err)
	assert.Equal(t, second.ID, got.PrivateIPID)
}

func TestFakeListsOnlyTheNamedVNICsAddresses(t *testing.T) {
	ctx := context.Background()
	f := oci.NewFake()

	_, err := f.AssignPrivateIP(ctx, "vnic1", netip.Addr{}, "spinifex-1")
	require.NoError(t, err)
	_, err = f.AssignPrivateIP(ctx, "vnic2", netip.Addr{}, "spinifex-2")
	require.NoError(t, err)

	got, err := f.ListPrivateIPs(ctx, "vnic1")
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Equal(t, "vnic1", got[0].VNICID)
}

func TestFakeUnassignAndDeleteRemoveTheObjects(t *testing.T) {
	ctx := context.Background()
	f := oci.NewFake()

	priv, err := f.AssignPrivateIP(ctx, "vnic1", netip.Addr{}, "spinifex-1")
	require.NoError(t, err)
	pub, err := f.CreatePublicIP(ctx, "comp1", priv.ID, "spinifex-1")
	require.NoError(t, err)

	require.NoError(t, f.DeletePublicIP(ctx, pub.ID))
	require.NoError(t, f.UnassignPrivateIP(ctx, priv.ID))
	assert.Empty(t, f.PublicIPs())
	assert.Empty(t, f.PrivateIPs())

	assert.ErrorIs(t, f.UnassignPrivateIP(ctx, priv.ID), oci.ErrNotFound)
	assert.ErrorIs(t, f.DeletePublicIP(ctx, pub.ID), oci.ErrNotFound)
}

// FailWith is how the rollback tests force a failure at an exact step, so it
// has to fire once and then clear — otherwise the cleanup call it triggers
// fails too and the test measures the wrong thing.
func TestFakeFailWithFiresOnceThenClears(t *testing.T) {
	ctx := context.Background()
	f := oci.NewFake()
	boom := errors.New("boom")
	f.FailWith["AssignPrivateIP"] = boom

	_, err := f.AssignPrivateIP(ctx, "vnic1", netip.Addr{}, "spinifex-1")
	assert.ErrorIs(t, err, boom)

	_, err = f.AssignPrivateIP(ctx, "vnic1", netip.Addr{}, "spinifex-1")
	assert.NoError(t, err, "a second call must succeed or rollback paths cannot be tested")
}

func TestFakeSeedHelpersExposeStateReconcileTestsNeed(t *testing.T) {
	f := oci.NewFake()
	f.SeedPrivateIP(oci.PrivateIP{ID: "p1", VNICID: "vnic1", DisplayName: "vnic-1", IsPrimary: true})
	f.SeedPublicIP(oci.PublicIP{ID: "q1", PrivateIPID: "p1"})

	assert.Len(t, f.PrivateIPs(), 1)
	assert.Len(t, f.PublicIPs(), 1)
	assert.True(t, f.PrivateIPs()[0].IsPrimary)
}
