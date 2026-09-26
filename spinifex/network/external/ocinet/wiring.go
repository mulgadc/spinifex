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

// FromPoolConfig builds a live allocator for one source="oci" pool: API-key
// auth from an ~/.oci/config profile, the VNIC resolved from the host interface
// when the config names one rather than an OCID, and the JetStream-backed
// bindings store.
//
// v1 authenticates from the config file rather than as an instance principal.
// It needs no dynamic group and no IAM policy from a tenancy admin, and it
// reads no instance metadata — which matters, because Spinifex's own IMDS
// endpoints claim 169.254.169.254 on the host and take the cloud's metadata
// service down with it. Configure oci_vnic_id rather than oci_vnic_iface for
// the same reason: resolving by interface is a metadata lookup.
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

	client, err := oci.NewConfigFileClient(pool.OCIConfigFile, pool.OCIConfigProfile)
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
