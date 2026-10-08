package addon

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/mulgadc/spinifex/spinifex/foundation/aws/arn"
	"github.com/nats-io/nats.go/jetstream"
)

// Owner is the sole writer of EKS managed add-on records and staged manifests.
// Every method takes the account's EKS bucket, which the caller has opened
// after confirming the cluster exists.
type Owner struct {
	region    string
	installer Installer
}

// New returns an Owner minting ARNs in region and delivering through installer.
func New(region string, installer Installer) *Owner {
	return &Owner{region: region, installer: installer}
}

// Desired is a validated create request: the catalogue-resolved name and
// version plus the operator-supplied desired state.
type Desired struct {
	Cluster               string
	Name                  string
	Version               string
	ServiceAccountRoleArn string
	ConfigurationValues   string
	Tags                  map[string]string
}

// Change is a validated update. Version applies when non-empty; the pointer
// fields apply when non-nil.
type Change struct {
	Version               string
	ConfigurationValues   *string
	ServiceAccountRoleArn *string
}

// Create stores a CREATING record and stages it for delivery. Returns ErrExists
// when a record already exists in any status. A staging failure leaves the
// record CREATE_FAILED and returns the staging error.
func (o *Owner) Create(ctx context.Context, kv jetstream.KeyValue, accountID string, d Desired) (*Record, error) {
	if _, err := Get(ctx, kv, d.Cluster, d.Name); err == nil {
		return nil, ErrExists
	} else if !errors.Is(err, ErrNotFound) {
		return nil, err
	}
	now := time.Now().UTC()
	rec := &Record{
		AddonName:             d.Name,
		AddonVersion:          d.Version,
		Status:                StatusCreating,
		ServiceAccountRoleArn: d.ServiceAccountRoleArn,
		ConfigurationValues:   d.ConfigurationValues,
		Arn:                   arn.FormatEKSAddon(o.region, accountID, d.Cluster, d.Name),
		Tags:                  d.Tags,
		CreatedAt:             now,
		ModifiedAt:            now,
	}
	if err := put(ctx, kv, d.Cluster, rec); err != nil {
		return nil, err
	}
	if err := o.installer.Install(ctx, accountID, d.Cluster, rec); err != nil {
		o.markFailed(ctx, kv, d.Cluster, d.Name, err)
		return nil, err
	}
	return rec, nil
}

// EnsureGPUDevicePlugin stages the bundled NVIDIA device plugin at its default
// version for a GPU node group's cluster. An existing record in any status is
// left untouched and is not an error.
func (o *Owner) EnsureGPUDevicePlugin(ctx context.Context, kv jetstream.KeyValue, accountID, cluster string) error {
	spec, ok := Lookup(NvidiaDevicePlugin)
	if !ok {
		return errors.New("eks: nvidia-device-plugin missing from add-on catalog")
	}
	_, err := o.Create(ctx, kv, accountID, Desired{Cluster: cluster, Name: spec.Name, Version: spec.DefaultVersion})
	if errors.Is(err, ErrExists) {
		return nil
	}
	return err
}

// Update applies c to the record from any stored status, marks it UPDATING and
// re-stages it. Returns ErrNotFound if absent; a staging failure leaves the
// record CREATE_FAILED and returns the staging error.
func (o *Owner) Update(ctx context.Context, kv jetstream.KeyValue, accountID, cluster, name string, c Change) (*Record, error) {
	now := time.Now().UTC()
	rec, err := casUpdate(ctx, kv, cluster, name, func(r *Record) bool {
		if c.Version != "" {
			r.AddonVersion = c.Version
		}
		if c.ConfigurationValues != nil {
			r.ConfigurationValues = *c.ConfigurationValues
		}
		if c.ServiceAccountRoleArn != nil {
			r.ServiceAccountRoleArn = *c.ServiceAccountRoleArn
		}
		r.Status = StatusUpdating
		r.ModifiedAt = now
		return true
	})
	if err != nil {
		return nil, err
	}
	if err := o.installer.Install(ctx, accountID, cluster, rec); err != nil {
		o.markFailed(ctx, kv, cluster, name, err)
		return nil, err
	}
	return rec, nil
}

// Delete unstages the manifest and erases the record and manifest, returning
// the record as it stood, marked DELETING. Returns ErrNotFound if absent.
func (o *Owner) Delete(ctx context.Context, kv jetstream.KeyValue, accountID, cluster, name string) (*Record, error) {
	rec, err := Get(ctx, kv, cluster, name)
	if err != nil {
		return nil, err
	}
	rec.Status = StatusDeleting
	rec.ModifiedAt = time.Now().UTC()
	if err := o.installer.Uninstall(ctx, accountID, cluster, name); err != nil {
		return nil, err
	}
	if err := deleteRecord(ctx, kv, cluster, name); err != nil {
		return nil, err
	}
	return rec, nil
}

// ApplyReport moves the named add-on to the status the report implies. A report
// naming no add-on or an absent one, or one that changes nothing, writes nothing.
func (o *Owner) ApplyReport(ctx context.Context, kv jetstream.KeyValue, cluster string, report Report) error {
	if report.Addon == "" {
		return nil
	}
	now := time.Now().UTC()
	_, err := casUpdate(ctx, kv, cluster, report.Addon, func(rec *Record) bool {
		next, changed := nextStatus(rec.Status, report.Phase)
		// The failure message becomes the health; recovery clears it.
		health := rec.Health
		switch report.Phase {
		case PhaseFailed:
			health = report.Message
		case PhaseReady:
			health = ""
		}
		if !changed && health == rec.Health {
			return false
		}
		rec.Status = next
		rec.Health = health
		rec.ModifiedAt = now
		return true
	})
	if errors.Is(err, ErrNotFound) {
		// Deleted: the guest drops the rendered manifest on its next sync.
		return nil
	}
	return err
}

// markFailed best-effort flips a record to CREATE_FAILED with the error reason.
func (o *Owner) markFailed(ctx context.Context, kv jetstream.KeyValue, cluster, name string, cause error) {
	now := time.Now().UTC()
	if _, err := casUpdate(ctx, kv, cluster, name, func(r *Record) bool {
		r.Status = StatusCreateFailed
		r.Health = cause.Error()
		r.ModifiedAt = now
		return true
	}); err != nil {
		slog.Warn("markAddonFailed: CAS failed", "cluster", cluster, "addon", name, "err", err)
	}
}

// nextStatus maps a delivery phase onto the next status and whether it changed:
// ready lifts any status to ACTIVE; failed degrades ACTIVE, keeps DEGRADED and
// CREATE_FAILED, else fails creation; applied and unknown phases change nothing.
func nextStatus(cur Status, phase Phase) (Status, bool) {
	switch phase {
	case PhaseReady:
		if cur == StatusActive {
			return cur, false
		}
		return StatusActive, true
	case PhaseFailed:
		switch cur {
		case StatusActive:
			return StatusDegraded, true
		case StatusDegraded, StatusCreateFailed:
			return cur, false
		default:
			return StatusCreateFailed, true
		}
	default:
		return cur, false
	}
}
