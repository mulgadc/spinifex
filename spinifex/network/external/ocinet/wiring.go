package ocinet

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/nats-io/nats.go/jetstream"

	"github.com/mulgadc/spinifex/spinifex/cloud/oci"
	"github.com/mulgadc/spinifex/spinifex/network/external"
	"github.com/mulgadc/spinifex/spinifex/network/host"
	"github.com/mulgadc/spinifex/spinifex/network/topology"
)

// FromPoolConfig builds a live allocator for one source="oci" pool: the auth
// method the pool names, the VNIC resolved from the host interface when the
// config names one rather than an OCID, and the JetStream-backed bindings store.
//
// It does not reconcile. The caller decides when that pass runs, because it
// deletes OCI objects and a startup path that does so before the store is
// readable would collect live addresses.
// sbAddr is the OVN Southbound address the gateway-chassis question is asked
// of; empty uses the local socket.
func FromPoolConfig(ctx context.Context, js jetstream.JetStream, pool external.ExternalPoolConfig, sbAddr string) (*PoolAllocator, error) {
	if !pool.IsOCI() {
		return nil, fmt.Errorf("ocinet: pool %q has source %q, not %q", pool.Name, pool.Source, external.SourceOCI)
	}

	client, err := newClient(pool)
	if err != nil {
		return nil, fmt.Errorf("ocinet: pool %q: %w", pool.Name, err)
	}

	vnicID, err := resolveVNICID(ctx, pool)
	if err != nil {
		return nil, err
	}

	compartmentID := pool.OCICompartmentID
	if compartmentID == "" {
		// Config validation requires the key, so this is the belt to that
		// brace rather than a supported configuration.
		inst, mdErr := oci.NewMetadataClient().Instance(ctx)
		if mdErr != nil {
			return nil, fmt.Errorf("ocinet: pool %q: no oci_compartment_id and metadata lookup failed: %w", pool.Name, mdErr)
		}
		compartmentID = inst.CompartmentID
	}

	return New(client, NewKVStore(js), Config{
		Pool:          pool,
		VNICID:        vnicID,
		CompartmentID: compartmentID,
		// Local OVS decides which addresses this node should hold, so the
		// affinity pass asks the host rather than any shared record.
		LocalPorts: func(ctx context.Context) (map[string]struct{}, error) {
			return host.ListLocalPorts(ctx, host.NewExecRunner())
		},
		// The same question for an address with no guest behind it: a NAT
		// gateway's belongs wherever its VPC's chassisredirect port is claimed,
		// which the local ovn-controller's own binding answers.
		LocalGateway: func(ctx context.Context, vpcID string) (bool, error) {
			return host.NewGatewayClaimProber(sbAddr).
				GatewayPortLocal(ctx, topology.GatewayChassisRedirectPort(vpcID))
		},
	})
}

// newClient authenticates the way the pool asks. Instance principal takes the
// credential from the instance's own certificate, so no key material sits on any
// node; a config file is the default because it needs nothing from a tenancy
// admin.
//
// Both read the metadata service on a pool configured with oci_vnic_iface, which
// is what the guide recommends, so instance principal adds no dependency on IMDS
// that such a node does not already have. The IMDS remap is what makes either
// work, since Spinifex's own endpoints otherwise claim 169.254.169.254.
func newClient(pool external.ExternalPoolConfig) (oci.Client, error) {
	if pool.UsesInstancePrincipal() {
		slog.Info("ocinet authenticating as the instance principal", "pool", pool.Name)
		return oci.NewInstancePrincipalClient()
	}
	return oci.NewConfigFileClient(pool.OCIConfigFile, pool.OCIConfigProfile)
}

// resolveVNICID turns whichever VNIC key the operator set into an OCID.
func resolveVNICID(ctx context.Context, pool external.ExternalPoolConfig) (string, error) {
	if pool.OCIVNICID != "" {
		return pool.OCIVNICID, nil
	}
	if pool.OCIVNICIface == "" {
		return "", fmt.Errorf("ocinet: pool %q sets neither oci_vnic_id nor oci_vnic_iface", pool.Name)
	}
	vnic, err := oci.ResolveVNICByInterface(ctx, oci.NewMetadataClient(), pool.OCIVNICIface)
	if err != nil {
		return "", fmt.Errorf("ocinet: pool %q: %w", pool.Name, err)
	}
	slog.InfoContext(ctx, "ocinet resolved the external VNIC from its interface",
		"pool", pool.Name, "iface", pool.OCIVNICIface, "vnic_id", vnic.VNICID,
		"private_ip", vnic.PrivateIP, "subnet", vnic.SubnetCIDRBlock)
	return vnic.VNICID, nil
}
