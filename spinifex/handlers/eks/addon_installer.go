package handlers_eks

import (
	"encoding/json"
	"errors"
	"fmt"

	"github.com/nats-io/nats.go"
)

// AddonInstaller delivers (and removes) a managed add-on's Kubernetes manifests
// to a cluster. It is deliberately transport-agnostic: the add-on state machine
// depends only on this narrow contract so the VM-side delivery transport (which
// drops manifests into the K3s server auto-deploy dir) can drop in without
// touching the op layer.
//
// The reachability constraint (cluster_reconciler.go:73-79) means delivery must
// be VM-side, not a daemon apiserver apply — a private cluster's apiserver is
// not host-reachable.
type AddonInstaller interface {
	// Install renders and delivers the add-on described by rec. It does not flip
	// the record to ACTIVE — that transition is gated on the cluster's state
	// report confirming the manifest applied and pods are ready.
	Install(accountID, cluster string, rec *AddonRecord) error
	// Uninstall removes the add-on's delivered manifests from the cluster.
	Uninstall(accountID, cluster, addon string) error
}

// stagedManifest is the artifact the VM-side delivery transport consumes. It
// names the bundled add-on + version and carries the operator-supplied config
// so the VM can render the final Kubernetes objects from the baked manifests.
type stagedManifest struct {
	AddonName             string `json:"addonName"`
	AddonVersion          string `json:"addonVersion"`
	ServiceAccountRoleArn string `json:"serviceAccountRoleArn,omitempty"`
	ConfigurationValues   string `json:"configurationValues,omitempty"`
}

// stagingInstaller renders the bundled manifest descriptor for an add-on and
// stages it in the per-account KV at AddonManifestKey. The VM-side delivery
// slice (separate bead) pulls staged manifests and applies them via the K3s
// auto-deploy dir; until then an installed add-on sits CREATING — honest, not a
// false ACTIVE.
type stagingInstaller struct {
	nc *nats.Conn
}

var _ AddonInstaller = (*stagingInstaller)(nil)

// newStagingInstaller builds the default installer bound to the daemon's NATS
// connection.
func newStagingInstaller(nc *nats.Conn) *stagingInstaller {
	return &stagingInstaller{nc: nc}
}

func (i *stagingInstaller) acctKV(accountID string) (nats.KeyValue, error) {
	if i.nc == nil {
		return nil, errors.New("eks: stagingInstaller nil NATS connection")
	}
	js, err := i.nc.JetStream()
	if err != nil {
		return nil, fmt.Errorf("jetstream: %w", err)
	}
	return GetOrCreateAccountBucket(js, accountID)
}

func (i *stagingInstaller) Install(accountID, cluster string, rec *AddonRecord) error {
	if rec == nil {
		return errors.New("eks: stagingInstaller Install nil record")
	}
	kv, err := i.acctKV(accountID)
	if err != nil {
		return err
	}
	manifest := stagedManifest{
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
	if _, err := kv.Put(key, data); err != nil {
		return fmt.Errorf("kv put %s: %w", key, err)
	}
	return nil
}

func (i *stagingInstaller) Uninstall(accountID, cluster, addon string) error {
	kv, err := i.acctKV(accountID)
	if err != nil {
		return err
	}
	key := AddonManifestKey(cluster, addon)
	if err := kv.Delete(key); err != nil && !errors.Is(err, nats.ErrKeyNotFound) {
		return fmt.Errorf("kv delete %s: %w", key, err)
	}
	return nil
}
