package ocinet_test

import (
	"context"
	"errors"
	"net/netip"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mulgadc/spinifex/spinifex/cloud/oci"
	"github.com/mulgadc/spinifex/spinifex/network/external"
	"github.com/mulgadc/spinifex/spinifex/network/external/ocinet"
)

const (
	thisVNIC  = "ocid1.vnic.oc1..vnic1"
	otherVNIC = "ocid1.vnic.oc1..vnic2"
)

// newAffinityAllocator builds an allocator whose VNIC is thisVNIC and whose
// idea of "running here" is the caller's port set.
func newAffinityAllocator(t *testing.T, fake *oci.Fake, local ...string) (*ocinet.PoolAllocator, *ocinet.MemStore) {
	t.Helper()
	ports := make(map[string]struct{}, len(local))
	for _, p := range local {
		ports[p] = struct{}{}
	}
	store := ocinet.NewMemStore()
	a, err := ocinet.New(fake, store, ocinet.Config{
		Pool:          external.ExternalPoolConfig{Name: "oci-wan", Source: external.SourceOCI},
		VNICID:        thisVNIC,
		CompartmentID: "ocid1.compartment.oc1..comp1",
		Schedule:      []time.Duration{time.Millisecond},
		Budget:        time.Second,
		Sleep:         func(context.Context, time.Duration) error { return nil },
		LocalPorts:    func(context.Context) (map[string]struct{}, error) { return ports, nil },
	})
	require.NoError(t, err)
	return a, store
}

// seedForeignBinding puts an allocated pair on otherVNIC and records it, which
// is the state after a guest has been started on a node other than the one that
// served its AllocateAddress.
func seedForeignBinding(t *testing.T, fake *oci.Fake, store *ocinet.MemStore, eniID string) (pubAddr string, privID string) {
	t.Helper()
	ctx := context.Background()
	priv, err := fake.AssignPrivateIP(ctx, otherVNIC, netip.Addr{}, ocinet.DisplayNamePrefix+eniID)
	require.NoError(t, err)
	pub, err := fake.CreatePublicIP(ctx, "ocid1.compartment.oc1..comp1", priv.ID, ocinet.DisplayNamePrefix+eniID)
	require.NoError(t, err)
	require.NoError(t, store.Mutate(ctx, "oci-wan", func(r *ocinet.Record) (bool, error) {
		r.Bindings[pub.Address.String()] = ocinet.Binding{
			PublicIPID: pub.ID, PrivateIPID: priv.ID, PrivateAddr: priv.Address.String(),
			ENIID: eniID, VNICID: otherVNIC,
		}
		return true, nil
	}))
	return pub.Address.String(), priv.ID
}

// The bug this pass exists for: the guest runs here, OCI delivers elsewhere, so
// the address is dark until the private half is moved onto this node's VNIC.
func TestClaimMovesAnAddressWhoseGuestRunsHere(t *testing.T) {
	ctx := context.Background()
	fake := oci.NewFake()
	a, store := newAffinityAllocator(t, fake, "port-eni-1")
	pubAddr, privID := seedForeignBinding(t, fake, store, "eni-1")

	res, err := a.ClaimLocalAddresses(ctx)
	require.NoError(t, err)
	assert.Equal(t, []string{pubAddr}, res.Claimed)

	got, err := fake.GetPrivateIP(ctx, privID)
	require.NoError(t, err)
	assert.Equal(t, thisVNIC, got.VNICID, "OCI still delivers this address to the old node")

	// The record must agree, or the startup reconcile reads the address as a
	// peer's and leaves a real leak of ours alone forever.
	rec, err := store.Get(ctx, "oci-wan")
	require.NoError(t, err)
	assert.Equal(t, thisVNIC, rec.Bindings[pubAddr].VNICID)
}

// The reserved public IP is the customer's address. A move that re-created the
// private IP would drop it; reassigning keeps the OCID, so the pairing holds.
func TestClaimKeepsThePublicAddressAndItsAttachment(t *testing.T) {
	ctx := context.Background()
	fake := oci.NewFake()
	a, store := newAffinityAllocator(t, fake, "port-eni-1")
	pubAddr, privID := seedForeignBinding(t, fake, store, "eni-1")

	_, err := a.ClaimLocalAddresses(ctx)
	require.NoError(t, err)

	pubs := fake.PublicIPs()
	require.Len(t, pubs, 1, "the reserved public IP must survive the move")
	assert.Equal(t, pubAddr, pubs[0].Address.String(), "the customer's address changed")
	assert.Equal(t, privID, pubs[0].PrivateIPID, "the public IP came detached from its private half")
}

// A node must never take an address whose guest is somewhere else. Getting this
// wrong is worse than the bug: it steals a working address from a live guest.
func TestClaimLeavesAnAddressWhoseGuestRunsElsewhere(t *testing.T) {
	ctx := context.Background()
	fake := oci.NewFake()
	// No local ports at all beyond an unrelated one.
	a, store := newAffinityAllocator(t, fake, "port-eni-somebody-else")
	_, privID := seedForeignBinding(t, fake, store, "eni-1")

	res, err := a.ClaimLocalAddresses(ctx)
	require.NoError(t, err)
	assert.Empty(t, res.Claimed)
	assert.Zero(t, res.Local)

	got, err := fake.GetPrivateIP(ctx, privID)
	require.NoError(t, err)
	assert.Equal(t, otherVNIC, got.VNICID, "took an address belonging to another node's guest")
}

// An allocated-but-unassociated EIP has no guest, so no node owns it. Claiming
// it would drag every free address onto whichever node swept last.
func TestClaimIgnoresAnUnassociatedAddress(t *testing.T) {
	ctx := context.Background()
	fake := oci.NewFake()
	a, store := newAffinityAllocator(t, fake, "port-eni-1")
	_, privID := seedForeignBinding(t, fake, store, "")

	res, err := a.ClaimLocalAddresses(ctx)
	require.NoError(t, err)
	assert.Empty(t, res.Claimed)

	got, err := fake.GetPrivateIP(ctx, privID)
	require.NoError(t, err)
	assert.Equal(t, otherVNIC, got.VNICID)
}

// Steady state is every pass on every node, so it must cost nothing once the
// addresses are where they belong.
func TestClaimMakesNoMoveCallWhenTheAddressIsAlreadyOurs(t *testing.T) {
	ctx := context.Background()
	fake := oci.NewFake()
	a, _ := newAffinityAllocator(t, fake, "port-eni-1")

	_, err := a.Allocate(ctx, external.AllocateRequest{PoolName: "oci-wan", ENIID: "eni-1"})
	require.NoError(t, err)

	// Any MovePrivateIP at all now is a needless write against a live address.
	fake.FailOp("MovePrivateIP", errors.New("must not be called"))

	res, err := a.ClaimLocalAddresses(ctx)
	require.NoError(t, err)
	assert.Empty(t, res.Claimed)
	assert.Equal(t, 1, res.Local, "the binding is this node's and should be counted")
}

// OVS is the only signal for "is this mine". A pass that guessed when it could
// not read it would either strand addresses or steal them.
func TestClaimFailsThePassWhenLocalOVSCannotBeRead(t *testing.T) {
	store := ocinet.NewMemStore()
	fake := oci.NewFake()
	a, err := ocinet.New(fake, store, ocinet.Config{
		Pool:          external.ExternalPoolConfig{Name: "oci-wan", Source: external.SourceOCI},
		VNICID:        thisVNIC,
		CompartmentID: "ocid1.compartment.oc1..comp1",
		LocalPorts: func(context.Context) (map[string]struct{}, error) {
			return nil, errors.New("ovs-vsctl: connection refused")
		},
	})
	require.NoError(t, err)

	_, err = a.ClaimLocalAddresses(context.Background())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "list local OVS ports")
}

// A single-node cluster and the allocator's own unit tests wire no port source;
// that must be a no-op rather than a nil dereference or a claim of everything.
func TestClaimIsANoOpWithoutALocalPortSource(t *testing.T) {
	ctx := context.Background()
	fake := oci.NewFake()
	a, _ := newTestAllocator(t, fake)

	res, err := a.ClaimLocalAddresses(ctx)
	require.NoError(t, err)
	assert.Empty(t, res.Claimed)
	assert.Zero(t, res.Local)
}

// The move succeeded against OCI before the record was written, so a failure to
// write it back must surface — silently keeping the old VNIC would have the
// startup reconcile treat this node's own address as a peer's.
func TestClaimReportsAFailureToRecordTheNewOwner(t *testing.T) {
	ctx := context.Background()
	fake := oci.NewFake()
	a, store := newAffinityAllocator(t, fake, "port-eni-1")
	seedForeignBinding(t, fake, store, "eni-1")

	store.FailMutate(errors.New("kv unavailable"))
	_, err := a.ClaimLocalAddresses(ctx)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "record new VNIC")
}

// newGatewayAffinityAllocator builds an allocator with no guests at all and a
// fixed answer to "am I the gateway chassis for this VPC".
func newGatewayAffinityAllocator(t *testing.T, fake *oci.Fake, local bool, err error) (*ocinet.PoolAllocator, *ocinet.MemStore) {
	t.Helper()
	store := ocinet.NewMemStore()
	a, aerr := ocinet.New(fake, store, ocinet.Config{
		Pool:          external.ExternalPoolConfig{Name: "oci-wan", Source: external.SourceOCI},
		VNICID:        thisVNIC,
		CompartmentID: "ocid1.compartment.oc1..comp1",
		Schedule:      []time.Duration{time.Millisecond},
		Budget:        time.Second,
		Sleep:         func(context.Context, time.Duration) error { return nil },
		LocalPorts:    func(context.Context) (map[string]struct{}, error) { return nil, nil },
		LocalGateway:  func(context.Context, string) (bool, error) { return local, err },
	})
	require.NoError(t, aerr)
	return a, store
}

// seedGatewayBinding puts an allocated pair on otherVNIC with no ENI and a VPC,
// which is a NAT gateway's address created by a node that does not forward it.
func seedGatewayBinding(t *testing.T, fake *oci.Fake, store *ocinet.MemStore, vpcID string) (pubAddr string, privID string) {
	t.Helper()
	ctx := context.Background()
	priv, err := fake.AssignPrivateIP(ctx, otherVNIC, netip.Addr{}, ocinet.DisplayNamePrefix+vpcID)
	require.NoError(t, err)
	pub, err := fake.CreatePublicIP(ctx, "ocid1.compartment.oc1..comp1", priv.ID, ocinet.DisplayNamePrefix+vpcID)
	require.NoError(t, err)
	require.NoError(t, store.Mutate(ctx, "oci-wan", func(r *ocinet.Record) (bool, error) {
		r.Bindings[pub.Address.String()] = ocinet.Binding{
			PublicIPID: pub.ID, PrivateIPID: priv.ID, PrivateAddr: priv.Address.String(),
			GatewayVPCID: vpcID, VNICID: otherVNIC,
		}
		return true, nil
	}))
	return pub.Address.String(), priv.ID
}

// The NAT gateway case: no guest, no ENI, and the address still has to be on
// the VNIC of the node that forwards for the VPC or OCI drops every packet.
func TestClaimMovesANATGatewayAddressToItsGatewayChassis(t *testing.T) {
	ctx := context.Background()
	fake := oci.NewFake()
	a, store := newGatewayAffinityAllocator(t, fake, true, nil)
	pubAddr, privID := seedGatewayBinding(t, fake, store, "vpc-1")

	res, err := a.ClaimLocalAddresses(ctx)
	require.NoError(t, err)
	assert.Equal(t, []string{pubAddr}, res.Claimed)

	got, err := fake.GetPrivateIP(ctx, privID)
	require.NoError(t, err)
	assert.Equal(t, thisVNIC, got.VNICID, "the SNAT source is not on the forwarding node's VNIC")

	rec, err := store.Get(ctx, "oci-wan")
	require.NoError(t, err)
	assert.Equal(t, thisVNIC, rec.Bindings[pubAddr].VNICID)
}

// Taking a gateway address from the node that forwards for it is the same theft
// as taking a guest's, and breaks every private subnet in that VPC.
func TestClaimLeavesANATGatewayAddressWhoseChassisIsElsewhere(t *testing.T) {
	ctx := context.Background()
	fake := oci.NewFake()
	a, store := newGatewayAffinityAllocator(t, fake, false, nil)
	_, privID := seedGatewayBinding(t, fake, store, "vpc-1")

	res, err := a.ClaimLocalAddresses(ctx)
	require.NoError(t, err)
	assert.Empty(t, res.Claimed)

	got, err := fake.GetPrivateIP(ctx, privID)
	require.NoError(t, err)
	assert.Equal(t, otherVNIC, got.VNICID)
}

// An unreadable chassis binding is not "not mine": answering no would leave a
// live gateway dark, so the pass fails and the next one asks again.
func TestClaimFailsWhenTheGatewayChassisCannotBeRead(t *testing.T) {
	ctx := context.Background()
	fake := oci.NewFake()
	a, store := newGatewayAffinityAllocator(t, fake, false, errors.New("ovn-sbctl: connection refused"))
	_, privID := seedGatewayBinding(t, fake, store, "vpc-1")

	_, err := a.ClaimLocalAddresses(ctx)
	require.Error(t, err)

	got, gerr := fake.GetPrivateIP(ctx, privID)
	require.NoError(t, gerr)
	assert.Equal(t, otherVNIC, got.VNICID, "a failed pass must not have moved anything")
}

// A bare EIP nobody has associated still has no node, and the gateway rule must
// not change that — claiming one would strand it on whichever node ran first.
func TestClaimStillLeavesAnUnassociatedEIPAlone(t *testing.T) {
	ctx := context.Background()
	fake := oci.NewFake()
	a, store := newGatewayAffinityAllocator(t, fake, true, nil)
	_, privID := seedGatewayBinding(t, fake, store, "")

	res, err := a.ClaimLocalAddresses(ctx)
	require.NoError(t, err)
	assert.Empty(t, res.Claimed)

	got, gerr := fake.GetPrivateIP(ctx, privID)
	require.NoError(t, gerr)
	assert.Equal(t, otherVNIC, got.VNICID)
}
