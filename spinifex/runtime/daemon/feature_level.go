package daemon

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"time"

	"github.com/mulgadc/spinifex/spinifex/foundation/release/featurelevel"
	instancecache "github.com/mulgadc/spinifex/spinifex/runtime/compute/cache"
	"github.com/nats-io/nats.go/jetstream"
)

// FeatureLevelStore returns the cluster feature-level record over the
// cluster-config bucket, which has no TTL.
func (m *JetStreamManager) FeatureLevelStore(ctx context.Context) (*featurelevel.Store, error) {
	if m.clusterConfig == nil {
		return nil, errors.New("cluster config KV not initialized")
	}
	kv, err := m.clusterConfig.KV(ctx)
	if err != nil {
		return nil, err
	}
	return featurelevel.NewStore(kv), nil
}

// featureLevelMembers reads every configured node's heartbeat. A member is
// current when its heartbeat is within NodeStaleAfter. A node with no heartbeat
// (never written, or expired from the bucket) or an unreadable timestamp is not
// current; any other read failure is returned so the tick advances nothing.
func featureLevelMembers(read func(node string) (*Heartbeat, error), nodes []string, now time.Time) ([]featurelevel.Member, error) {
	members := make([]featurelevel.Member, 0, len(nodes))
	for _, node := range nodes {
		h, err := read(node)
		if errors.Is(err, jetstream.ErrKeyNotFound) {
			members = append(members, featurelevel.Member{Node: node})
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("read heartbeat of %s: %w", node, err)
		}
		written, perr := time.Parse(time.RFC3339, h.Timestamp)
		current := perr == nil && now.UTC().Sub(written.UTC()) <= instancecache.NodeStaleAfter
		members = append(members, featurelevel.Member{Node: node, Level: h.FeatureLevel, Current: current})
	}
	return members, nil
}

// advanceFeatureLevel raises the recorded level when every configured node has
// a current heartbeat reporting it. Any number of daemons may call it.
func advanceFeatureLevel(ctx context.Context, store *featurelevel.Store, read func(string) (*Heartbeat, error), nodes []string, now time.Time) (featurelevel.Record, bool, error) {
	members, err := featureLevelMembers(read, nodes, now)
	if err != nil {
		return featurelevel.Record{}, false, err
	}
	return store.Advance(ctx, members)
}

// tickFeatureLevel runs on each heartbeat tick. A failure is logged and never
// fatal: the level stays where it is until a later tick succeeds.
func (d *Daemon) tickFeatureLevel() {
	if d.jsManager == nil || d.clusterConfig == nil {
		return
	}
	ctx, cancel := context.WithTimeout(d.ctx, heartbeatInterval)
	defer cancel()
	store, err := d.jsManager.FeatureLevelStore(ctx)
	if err != nil {
		slog.Warn("Feature level: store unavailable", "error", err)
		return
	}
	nodes := slices.Sorted(maps.Keys(d.clusterConfig.Nodes))
	rec, advanced, err := advanceFeatureLevel(ctx, store, d.jsManager.ReadHeartbeat, nodes, time.Now())
	if err != nil {
		slog.Warn("Feature level: advance failed", "error", err)
		return
	}
	if advanced {
		slog.Info("Cluster feature level advanced", "level", rec.Level, "build", featurelevel.Build)
	}
}
