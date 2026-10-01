package exonet

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/nats-io/nats.go/jetstream"

	"github.com/mulgadc/spinifex/spinifex/network/external"
	"github.com/mulgadc/spinifex/spinifex/providers/cloud/exoscale"
)

// FromPoolConfig builds a live allocator for one source="exoscale" pool and
// proves the exo binary runs before anything depends on it. It does not
// reconcile; the caller runs that pass before the allocator serves.
func FromPoolConfig(ctx context.Context, js jetstream.JetStream, pool external.ExternalPoolConfig) (*PoolAllocator, error) {
	if !pool.IsExoscale() {
		return nil, fmt.Errorf("exonet: pool %q has source %q, not %q", pool.Name, pool.Source, external.SourceExoscale)
	}
	client, err := exoscale.NewCLIClient(exoscale.CLIConfig{
		Binary:     pool.ExoscaleBinary,
		ConfigFile: pool.ExoscaleConfigFile,
		Account:    pool.ExoscaleAccount,
		Zone:       pool.ExoscaleZone,
	})
	if err != nil {
		return nil, fmt.Errorf("exonet: pool %q: %w", pool.Name, err)
	}
	version, err := client.Version(ctx)
	if err != nil {
		return nil, fmt.Errorf("exonet: pool %q: exo CLI is not usable: %w", pool.Name, err)
	}
	slog.InfoContext(ctx, "exonet using exo CLI", "pool", pool.Name, "version", version,
		"zone", pool.ExoscaleZone, "instance_id", pool.ExoscaleInstanceID)
	return New(client, NewKVStore(js), Config{Pool: pool, InstanceID: pool.ExoscaleInstanceID})
}
