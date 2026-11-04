package floating

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/nats-io/nats.go/jetstream"

	"github.com/mulgadc/spinifex/spinifex/domains/network/external"
	"github.com/mulgadc/spinifex/spinifex/domains/network/host"
	"github.com/mulgadc/spinifex/spinifex/domains/network/topology"
	"github.com/mulgadc/spinifex/spinifex/providers/cloud/latitude"
	"github.com/mulgadc/spinifex/spinifex/providers/cloud/vultr"
)

// FromPoolConfig builds a live allocator for one floating pool and proves the
// credential works before anything depends on it. It does not reconcile; the
// caller runs that pass. sbAddr is the OVN Southbound address the
// gateway-chassis question is asked of; empty uses the local socket. peerHostIDs are
// the other nodes' provider host IDs, the only holders a log may name.
func FromPoolConfig(ctx context.Context, js jetstream.JetStream, pool external.ExternalPoolConfig, sbAddr string, peerHostIDs []string) (*PoolAllocator, error) {
	provider, err := providerFor(pool)
	if err != nil {
		return nil, err
	}
	return fromProvider(ctx, js, pool, sbAddr, provider, peerHostIDs)
}

// providerFor builds the pool's provider from its source.
func providerFor(pool external.ExternalPoolConfig) (Provider, error) {
	switch {
	case pool.IsVultr():
		client, err := vultr.New(vultr.Config{Region: pool.VultrRegion, KeyFile: pool.VultrKeyFile})
		if err != nil {
			return nil, fmt.Errorf("floating: pool %q: %w", pool.Name, err)
		}
		return NewVultrProvider(client), nil
	case pool.IsLatitude():
		client, err := latitude.New(latitude.Config{ProjectID: pool.LatitudeProjectID, KeyFile: pool.LatitudeKeyFile})
		if err != nil {
			return nil, fmt.Errorf("floating: pool %q: %w", pool.Name, err)
		}
		p, err := NewLatitudeProvider(client, pool.LatitudeElasticIPIDs)
		if err != nil {
			return nil, fmt.Errorf("floating: pool %q: %w", pool.Name, err)
		}
		return p, nil
	}
	return nil, fmt.Errorf("floating: pool %q has source %q, not a floating provider", pool.Name, pool.Source)
}

func fromProvider(ctx context.Context, js jetstream.JetStream, pool external.ExternalPoolConfig, sbAddr string, provider Provider, peerHostIDs []string) (*PoolAllocator, error) {
	// A refused key looks ready until the first launch, so the refusal is
	// surfaced at start with the provider's reason.
	_, err := provider.List(ctx)
	reportFirstCall(ctx, pool, err)

	return New(provider, NewKVStore(js), Config{
		Pool:         pool,
		HostID:       pool.ProviderHostID,
		KnownHostIDs: peerHostIDs,
		LocalPorts: func(ctx context.Context) (map[string]struct{}, error) {
			return host.ListLocalPorts(ctx, host.NewExecRunner())
		},
		LocalGateway: func(ctx context.Context, vpcID string) (bool, error) {
			return host.NewGatewayClaimProber(sbAddr).
				GatewayPortLocal(ctx, topology.GatewayChassisRedirectPort(vpcID))
		},
	})
}

// reportFirstCall logs the outcome of the pool's first provider call. The Vultr
// lines are matched by the deploy script, so their text must not change.
func reportFirstCall(ctx context.Context, pool external.ExternalPoolConfig, err error) {
	switch {
	case pool.IsVultr() && err != nil:
		slog.ErrorContext(ctx, "floating: Vultr refused the pool's first call; public addresses will fail until fixed",
			"pool", pool.Name, "region", pool.VultrRegion, "error", err)
	case pool.IsVultr():
		slog.InfoContext(ctx, "floating using Vultr reserved IPs", "pool", pool.Name,
			"region", pool.VultrRegion, "provider_host_id", pool.ProviderHostID, "label_prefix", pool.LabelPrefix)
	case err != nil:
		slog.ErrorContext(ctx, "floating: the provider refused the pool's first call; public addresses will fail until fixed",
			"pool", pool.Name, "source", pool.Source, "error", err)
	default:
		slog.InfoContext(ctx, "floating pool ready", "pool", pool.Name, "source", pool.Source,
			"provider_host_id", pool.ProviderHostID)
	}
}
