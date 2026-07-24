package handlers_eks

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
)

// AddonInstaller delivers and removes a managed add-on's Kubernetes manifests.
// Transport-agnostic: delivery must be VM-side because a private cluster's
// apiserver is not host-reachable.
type AddonInstaller interface {
	// Install stages the add-on manifest. Does not transition to ACTIVE — that
	// is gated on the cluster's state report confirming pods are ready.
	Install(ctx context.Context, accountID, cluster string, rec *AddonRecord) error
	// Uninstall removes the add-on's manifests from the cluster.
	Uninstall(ctx context.Context, accountID, cluster, addon string) error
}

// StagedAddonManifest is the artifact the VM-side delivery transport consumes.
// It names the bundled add-on + version and carries the operator-supplied config
// so the VM can render the final Kubernetes objects from the baked manifests.
// It is the wire shape the on-VM addon-sync agent fetches via
// GET /clusters/{name}/internal-addons.
type StagedAddonManifest struct {
	AddonName             string `json:"addonName"`
	AddonVersion          string `json:"addonVersion"`
	ServiceAccountRoleArn string `json:"serviceAccountRoleArn,omitempty"`
	ConfigurationValues   string `json:"configurationValues,omitempty"`
}

// stagingInstaller stages the manifest descriptor in KV at AddonManifestKey.
// The VM-side delivery slice applies it via the K3s auto-deploy dir; until then
// the add-on sits CREATING.
type stagingInstaller struct {
	nc          *nats.Conn
	clusterSize int
}

var _ AddonInstaller = (*stagingInstaller)(nil)

// newStagingInstaller returns a stagingInstaller bound to the daemon NATS
// connection, using clusterSize as the per-account bucket's replica count.
func newStagingInstaller(nc *nats.Conn, clusterSize int) *stagingInstaller {
	return &stagingInstaller{nc: nc, clusterSize: clusterSize}
}

func (i *stagingInstaller) acctKV(ctx context.Context, accountID string) (jetstream.KeyValue, error) {
	if i.nc == nil {
		return nil, errors.New("eks: stagingInstaller nil NATS connection")
	}
	js, err := jetstream.New(i.nc)
	if err != nil {
		return nil, fmt.Errorf("jetstream: %w", err)
	}
	return GetOrCreateAccountBucket(ctx, js, accountID, max(i.clusterSize, 1))
}

func (i *stagingInstaller) Install(ctx context.Context, accountID, cluster string, rec *AddonRecord) error {
	if rec == nil {
		return errors.New("eks: stagingInstaller Install nil record")
	}
	kv, err := i.acctKV(ctx, accountID)
	if err != nil {
		return err
	}
	manifest := StagedAddonManifest{
		AddonName:             rec.AddonName,
		AddonVersion:          rec.AddonVersion,
		ServiceAccountRoleArn: rec.ServiceAccountRoleArn,
		ConfigurationValues:   rec.ConfigurationValues,
	}
	data, err := json.Marshal(&manifest)
	if err != nil {
		return fmt.Errorf("marshal staged manifest %s: %w", rec.AddonName, err)
	}
	key := AddonManifestKey(cluster, rec.AddonName)
	if _, err := kv.Put(ctx, key, data); err != nil {
		return fmt.Errorf("kv put %s: %w", key, err)
	}
	return nil
}

func (i *stagingInstaller) Uninstall(ctx context.Context, accountID, cluster, addon string) error {
	kv, err := i.acctKV(ctx, accountID)
	if err != nil {
		return err
	}
	key := AddonManifestKey(cluster, addon)
	if err := kv.Delete(ctx, key); err != nil && !errors.Is(err, jetstream.ErrKeyNotFound) {
		return fmt.Errorf("kv delete %s: %w", key, err)
	}
	return nil
}
