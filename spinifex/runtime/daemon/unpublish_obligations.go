package daemon

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"

	ebsobligation "github.com/mulgadc/spinifex/spinifex/domains/ec2/ebs/obligation"
	ec2volume "github.com/mulgadc/spinifex/spinifex/domains/ec2/volume"
	"github.com/nats-io/nats.go/jetstream"
)

var _ ec2volume.InstanceNodeLocator = (*JetStreamManager)(nil)

// volumeMounter returns this node's one mounter. The VM manager and the
// unpublish obligation reaper must share it, because its volume locks are what
// keep the reaper's check and unpublish clear of a publish on this node.
func (d *Daemon) volumeMounter() *volumeMounterAdapter {
	d.mounterOnce.Do(func() {
		d.mounter = newVolumeMounterAdapter(d.ebsProvider, d.node, d.volumeService)
	})
	return d.mounter
}

// configureUnpublishObligations opens the obligation bucket and gives the
// volume service what a cleared attachment needs to record one. A node that
// cannot open it fails startup rather than its first force detach.
func (d *Daemon) configureUnpublishObligations() error {
	store, err := initServiceWithRetry("EBS unpublish obligation bucket", func() (*ebsobligation.Store, error) {
		js, err := jetstream.New(d.natsConn)
		if err != nil {
			return nil, fmt.Errorf("jetstream handle: %w", err)
		}
		store := ebsobligation.NewStore(js)
		if err := store.Open(d.ctx); err != nil {
			return nil, err
		}
		return store, nil
	})
	if err != nil {
		return err
	}
	var locator ec2volume.InstanceNodeLocator
	if d.jsManager != nil {
		locator = d.jsManager
	}
	d.volumeService.SetUnpublishObligations(store, locator, d.clusterMembers)
	return nil
}

// clusterMembers names every node in this node's cluster configuration.
func (d *Daemon) clusterMembers() []string {
	if d.clusterConfig == nil {
		return nil
	}
	return slices.Sorted(maps.Keys(d.clusterConfig.Nodes))
}

// nodeDeparted reports whether node has been removed from the cluster. A node
// that is only down, stale or partitioned is still a member and keeps its
// obligations. One this node's configuration lacks counts as removed only if
// its heartbeat is gone or older than this node's epoch: a heartbeat at this
// epoch or later means this node's view may be the stale one.
func (d *Daemon) nodeDeparted(_ context.Context, node string) (bool, error) {
	if d.clusterConfig == nil || d.jsManager == nil {
		return false, nil
	}
	if _, member := d.clusterConfig.Nodes[node]; member {
		return false, nil
	}
	heartbeat, err := d.jsManager.ReadHeartbeat(node)
	if errors.Is(err, jetstream.ErrKeyNotFound) {
		return true, nil
	}
	if err != nil {
		return false, fmt.Errorf("read heartbeat of %s: %w", node, err)
	}
	return heartbeat.Epoch < d.clusterConfig.Epoch, nil
}
