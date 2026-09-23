package ocinet

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/nats-io/nats.go/jetstream"

	"github.com/mulgadc/spinifex/spinifex/cloud/oci"
	"github.com/mulgadc/spinifex/spinifex/network/external"
)

// FromPoolConfig builds a live allocator for one source="oci" pool: instance
// principal auth, the VNIC resolved from the host interface when the config
// names one rather than an OCID, and the JetStream-backed bindings store.
//
// It does not reconcile. The caller decides when that pass runs, because it
// deletes OCI objects and a startup path that does so before the store is
// readable would collect live addresses.
func FromPoolConfig(ctx context.Context, js jetstream.JetStream, pool external.ExternalPoolConfig) (*PoolAllocator, error) {
	if !pool.IsOCI() {
		return nil, fmt.Errorf("ocinet: pool %q has source %q, not %q", pool.Name, pool.Source, external.SourceOCI)
	}

	client, err := oci.NewInstancePrincipalClient()
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
