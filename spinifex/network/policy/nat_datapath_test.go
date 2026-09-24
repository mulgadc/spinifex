//test:in-package — reuses nat_test.go's in-package seedRouter and findNAT
//fixtures, which drive the same mock NB client these cases assert against.

package policy

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mulgadc/spinifex/spinifex/network/ovn/mock"
)

// pairResolver maps one public address to its on-wire private one, the shape
// an OCI address pair takes.
func pairResolver(public, private string) DatapathResolver {
	return func(_ context.Context, ip string) (string, error) {
		if ip == public {
			return private, nil
		}
		return ip, nil
	}
}

// The public half of an OCI pair is NAT'd upstream and never reaches the wire,
// so a rule carrying it matches nothing and the instance is dark.
func TestNATManager_AddEIP_RowCarriesTheDatapathAddress(t *testing.T) {
	ctx := context.Background()
	m := mock.New()
	seedRouter(t, m, "vpc-1")
	nm, err := NewNATManager(m, NATModeCentralized,
		WithDatapathResolver(pairResolver("203.0.113.9", "10.200.0.183")))
	require.NoError(t, err)

	require.NoError(t, nm.AddEIP(ctx, EIPSpec{
		VPCID: "vpc-1", ExternalIP: "203.0.113.9", LogicalIP: "172.31.0.4",
		PortName: "port-eni-abc", MAC: "aa:bb:cc:dd:ee:ff",
	}))

	row := findNAT(m, "dnat_and_snat", "172.31.0.4")
	require.NotNil(t, row)
	assert.Equal(t, "10.200.0.183", row.ExternalIP,
		"OVN must match the address OCI actually puts on the VNIC")
	assert.Equal(t, "203.0.113.9", row.ExternalIDs["spinifex:public_ip"],
		"the public half is what an operator reads the instance back as")
}

// Intent is built from AWS records and names public addresses; the rows hold
// private ones. Compared unmapped, every live OCI instance looks orphaned.
func TestNATManager_PruneOrphanEIPs_KeepsALiveMappedAddress(t *testing.T) {
	ctx := context.Background()
	m := mock.New()
	seedRouter(t, m, "vpc-1")
	nm, err := NewNATManager(m, NATModeCentralized,
		WithDatapathResolver(pairResolver("203.0.113.9", "10.200.0.183")))
	require.NoError(t, err)

	spec := EIPSpec{
		VPCID: "vpc-1", ExternalIP: "203.0.113.9", LogicalIP: "172.31.0.4",
		PortName: "port-eni-abc", MAC: "aa:bb:cc:dd:ee:ff",
	}
	require.NoError(t, nm.AddEIP(ctx, spec))

	pruned, err := nm.PruneOrphanEIPs(ctx, LiveEIPs{
		Ports:       map[string]struct{}{"port-eni-abc": {}},
		ExternalIPs: map[string]struct{}{"203.0.113.9": {}},
	})
	require.NoError(t, err)
	assert.Zero(t, pruned, "a live instance must survive the sweep")
	assert.NotNil(t, findNAT(m, "dnat_and_snat", "172.31.0.4"))
}

// A delete naming the public half would find neither the row nor the host
// plumbing, and the address would go back to the pool still wired up.
func TestNATManager_DeleteEIP_RemovesTheMappedRow(t *testing.T) {
	ctx := context.Background()
	m := mock.New()
	seedRouter(t, m, "vpc-1")
	nm, err := NewNATManager(m, NATModeCentralized,
		WithDatapathResolver(pairResolver("203.0.113.9", "10.200.0.183")))
	require.NoError(t, err)

	require.NoError(t, nm.AddEIP(ctx, EIPSpec{
		VPCID: "vpc-1", ExternalIP: "203.0.113.9", LogicalIP: "172.31.0.4",
		PortName: "port-eni-abc", MAC: "aa:bb:cc:dd:ee:ff",
	}))
	require.NoError(t, nm.DeleteEIP(ctx, "vpc-1", "203.0.113.9", "172.31.0.4", "port-eni-abc"))

	assert.Nil(t, findNAT(m, "dnat_and_snat", "172.31.0.4"))
}

// Falling back to the public address on a lookup failure installs a rule that
// is indistinguishable from a working one until the instance is unreachable.
func TestNATManager_AddEIP_ResolverFailureIsFatal(t *testing.T) {
	ctx := context.Background()
	m := mock.New()
	seedRouter(t, m, "vpc-1")
	nm, err := NewNATManager(m, NATModeCentralized,
		WithDatapathResolver(func(context.Context, string) (string, error) {
			return "", errors.New("bindings unavailable")
		}))
	require.NoError(t, err)

	err = nm.AddEIP(ctx, EIPSpec{
		VPCID: "vpc-1", ExternalIP: "203.0.113.9", LogicalIP: "172.31.0.4",
		PortName: "port-eni-abc", MAC: "aa:bb:cc:dd:ee:ff",
	})
	require.Error(t, err)
	assert.Nil(t, findNAT(m, "dnat_and_snat", "172.31.0.4"))
}

// Half a mapped comparison sweeps exactly the addresses it could not resolve.
func TestNATManager_PruneOrphanEIPs_ResolverFailureAbortsTheSweep(t *testing.T) {
	ctx := context.Background()
	m := mock.New()
	seedRouter(t, m, "vpc-1")
	nm, err := NewNATManager(m, NATModeCentralized)
	require.NoError(t, err)
	require.NoError(t, nm.AddEIP(ctx, EIPSpec{
		VPCID: "vpc-1", ExternalIP: "203.0.113.9", LogicalIP: "172.31.0.4",
		PortName: "port-eni-abc", MAC: "aa:bb:cc:dd:ee:ff",
	}))

	broken, err := NewNATManager(m, NATModeCentralized,
		WithDatapathResolver(func(context.Context, string) (string, error) {
			return "", errors.New("bindings unavailable")
		}))
	require.NoError(t, err)

	_, err = broken.PruneOrphanEIPs(ctx, LiveEIPs{
		Ports:       map[string]struct{}{"port-eni-abc": {}},
		ExternalIPs: map[string]struct{}{"203.0.113.9": {}},
	})
	require.Error(t, err)
	assert.NotNil(t, findNAT(m, "dnat_and_snat", "172.31.0.4"),
		"an unresolvable sweep must change nothing")
}
