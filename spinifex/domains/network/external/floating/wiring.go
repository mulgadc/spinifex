package floating

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/nats-io/nats.go/jetstream"

	"github.com/mulgadc/spinifex/spinifex/domains/network/external"
	"github.com/mulgadc/spinifex/spinifex/domains/network/host"
	"github.com/mulgadc/spinifex/spinifex/domains/network/topology"
	"github.com/mulgadc/spinifex/spinifex/providers/cloud/vultr"
)

// FromPoolConfig builds a live allocator for one floating pool and proves the
// credential works before anything depends on it. It does not reconcile; the
// caller runs that pass. sbAddr is the OVN Southbound address the
// gateway-chassis question is asked of; empty uses the local socket.
func FromPoolConfig(ctx context.Context, js jetstream.JetStream, pool external.ExternalPoolConfig, sbAddr string) (*PoolAllocator, error) {
	if !pool.IsVultr() {
		return nil, fmt.Errorf("floating: pool %q has source %q, not a floating provider", pool.Name, pool.Source)
	}
	client, err := vultr.New(vultr.Config{Region: pool.VultrRegion, KeyFile: pool.VultrKeyFile})
	if err != nil {
		return nil, fmt.Errorf("floating: pool %q: %w", pool.Name, err)
	}
	provider := NewVultrProvider(client)

	// An allowlisted key refused from this node's address looks ready until the
	// first launch, so the refusal is surfaced at start with Vultr's reason.
	if _, err := provider.List(ctx); err != nil {
		slog.ErrorContext(ctx, "floating: Vultr refused the pool's first call; public addresses will fail until fixed",
			"pool", pool.Name, "region", pool.VultrRegion, "error", err)
	} else {
		slog.InfoContext(ctx, "floating using Vultr reserved IPs", "pool", pool.Name,
			"region", pool.VultrRegion, "provider_host_id", pool.ProviderHostID, "label_prefix", pool.LabelPrefix)
	}

	return New(provider, NewKVStore(js), Config{
		Pool:   pool,
		HostID: pool.ProviderHostID,
		LocalPorts: func(ctx context.Context) (map[string]struct{}, error) {
			return host.ListLocalPorts(ctx, host.NewExecRunner())
		},
		LocalGateway: func(ctx context.Context, vpcID string) (bool, error) {
			return host.NewGatewayClaimProber(sbAddr).
				GatewayPortLocal(ctx, topology.GatewayChassisRedirectPort(vpcID))
		},
	})
}
