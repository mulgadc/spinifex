package addon

import (
	"context"
	"errors"

	"github.com/nats-io/nats.go/jetstream"
)

// Installer delivers and removes a managed add-on's Kubernetes manifests.
// Delivery must be guest-side because a private cluster's apiserver is not
// host-reachable.
type Installer interface {
	// Install stages the add-on manifest. ACTIVE waits on a guest ready report.
	Install(ctx context.Context, accountID, cluster string, rec *Record) error
	// Uninstall removes the add-on's manifests from the cluster.
	Uninstall(ctx context.Context, accountID, cluster, addon string) error
}

// Bucket opens the account's EKS bucket, which holds the add-on keys.
type Bucket func(ctx context.Context, accountID string) (jetstream.KeyValue, error)

// StagingInstaller stages the manifest descriptor in KV at ManifestKey, where
// the guest addon-sync agent fetches it; the add-on sits CREATING until the
// guest reports it ready.
type StagingInstaller struct {
	bucket Bucket
}

var _ Installer = (*StagingInstaller)(nil)

// NewStagingInstaller returns a StagingInstaller writing to the bucket opened
// by bucket.
func NewStagingInstaller(bucket Bucket) *StagingInstaller {
	return &StagingInstaller{bucket: bucket}
}

// Install writes (or overwrites) the add-on's staged manifest.
func (i *StagingInstaller) Install(ctx context.Context, accountID, cluster string, rec *Record) error {
	if rec == nil {
		return errors.New("eks: stagingInstaller Install nil record")
	}
	kv, err := i.bucket(ctx, accountID)
	if err != nil {
		return err
	}
	return putManifest(ctx, kv, cluster, rec)
}

// Uninstall removes the add-on's staged manifest; an absent one is not an error.
func (i *StagingInstaller) Uninstall(ctx context.Context, accountID, cluster, addon string) error {
	kv, err := i.bucket(ctx, accountID)
	if err != nil {
		return err
	}
	return deleteManifest(ctx, kv, cluster, addon)
}

// Phase is the guest-observed delivery phase reported for one add-on.
type Phase string

const (
	// PhaseApplied: the rendered manifest was written but pods are not yet Ready.
	PhaseApplied Phase = "applied"
	// PhaseReady: the add-on's workloads rolled out successfully.
	PhaseReady Phase = "ready"
	// PhaseFailed: render or rollout failed.
	PhaseFailed Phase = "failed"
)

// Report is one guest delivery observation for the named add-on. The guest's
// reported version and timestamp are not part of it: neither is read today.
type Report struct {
	Addon   string
	Phase   Phase
	Message string
}
