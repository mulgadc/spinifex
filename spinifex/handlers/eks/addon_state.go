package handlers_eks

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/nats-io/nats.go/jetstream"
)

// AddonStatus mirrors the AWS EKS addon.status enum verbatim.
type AddonStatus string

const (
	AddonStatusCreating     AddonStatus = "CREATING"
	AddonStatusActive       AddonStatus = "ACTIVE"
	AddonStatusUpdating     AddonStatus = "UPDATING"
	AddonStatusDeleting     AddonStatus = "DELETING"
	AddonStatusDegraded     AddonStatus = "DEGRADED"
	AddonStatusCreateFailed AddonStatus = "CREATE_FAILED"
)

// AddonRecord is the persisted state for a managed add-on: lifecycle status,
// IRSA role, and opaque configuration the installer needs.
type AddonRecord struct {
	AddonName             string            `json:"addonName"`
	AddonVersion          string            `json:"addonVersion"`
	Status                AddonStatus       `json:"status"`
	ServiceAccountRoleArn string            `json:"serviceAccountRoleArn,omitempty"`
	ConfigurationValues   string            `json:"configurationValues,omitempty"`
	Health                string            `json:"health,omitempty"`
	Arn                   string            `json:"arn"`
	Tags                  map[string]string `json:"tags,omitempty"`
	CreatedAt             time.Time         `json:"createdAt"`
	ModifiedAt            time.Time         `json:"modifiedAt"`
}

// ErrAddonNotFound is returned when no record exists for the add-on.
// Callers translate it to ResourceNotFoundException at the service boundary.
var ErrAddonNotFound = errors.New("eks: addon not found")

// AddonARN composes the deterministic ARN for a managed add-on.
func AddonARN(region, accountID, cluster, addon string) string {
	return fmt.Sprintf("arn:aws:eks:%s:%s:addon/%s/%s", region, accountID, cluster, addon)
}

// PutAddonRecord writes the record unconditionally.
func PutAddonRecord(ctx context.Context, kv jetstream.KeyValue, cluster string, rec *AddonRecord) error {
	if rec == nil {
		return errors.New("eks: PutAddonRecord nil record")
	}
	if cluster == "" || rec.AddonName == "" {
		return errors.New("eks: PutAddonRecord missing cluster or addon name")
	}
	data, err := json.Marshal(rec)
	if err != nil {
		return fmt.Errorf("marshal addon %s: %w", rec.AddonName, err)
	}
	key := AddonKey(cluster, rec.AddonName)
	if _, err := kv.Put(ctx, key, data); err != nil {
		return fmt.Errorf("kv put %s: %w", key, err)
	}
	return nil
}

// GetAddonRecord reads one record. Returns ErrAddonNotFound if absent.
func GetAddonRecord(ctx context.Context, kv jetstream.KeyValue, cluster, addon string) (*AddonRecord, error) {
	if cluster == "" || addon == "" {
		return nil, errors.New("eks: GetAddonRecord empty cluster or addon name")
	}
	entry, err := kv.Get(ctx, AddonKey(cluster, addon))
	if err != nil {
		if errors.Is(err, jetstream.ErrKeyNotFound) {
			return nil, ErrAddonNotFound
		}
		return nil, fmt.Errorf("kv get addon: %w", err)
	}
	var rec AddonRecord
	if err := json.Unmarshal(entry.Value(), &rec); err != nil {
		return nil, fmt.Errorf("unmarshal addon %s: %w", addon, err)
	}
	return &rec, nil
}

// ListAddonRecords returns all add-on records under a cluster, sorted by name.
// Staged-manifest sub-keys (one extra path segment) are skipped.
func ListAddonRecords(ctx context.Context, kv jetstream.KeyValue, cluster string) ([]*AddonRecord, error) {
	if cluster == "" {
		return nil, errors.New("eks: ListAddonRecords empty cluster")
	}
	keys, err := kv.Keys(ctx)
	if err != nil {
		if errors.Is(err, jetstream.ErrNoKeysFound) {
			return nil, nil
		}
		return nil, fmt.Errorf("kv keys: %w", err)
	}
	prefix := AddonsPrefix(cluster)
	out := make([]*AddonRecord, 0)
	for _, k := range keys {
		if !strings.HasPrefix(k, prefix) {
			continue
		}
		// Skip sub-keys (e.g. staged manifest); record keys are one segment under the prefix.
		if strings.Contains(strings.TrimPrefix(k, prefix), "/") {
			continue
		}
		entry, err := kv.Get(ctx, k)
		if err != nil {
			if errors.Is(err, jetstream.ErrKeyNotFound) {
				continue
			}
			return nil, fmt.Errorf("kv get %s: %w", k, err)
		}
		var rec AddonRecord
		if err := json.Unmarshal(entry.Value(), &rec); err != nil {
			return nil, fmt.Errorf("unmarshal addon %s: %w", k, err)
		}
		out = append(out, &rec)
	}
	sortAddonRecords(out)
	return out, nil
}

// DeleteAddonRecord removes one record and its staged manifest. Returns ErrAddonNotFound if absent.
func DeleteAddonRecord(ctx context.Context, kv jetstream.KeyValue, cluster, addon string) error {
	key := AddonKey(cluster, addon)
	if _, err := kv.Get(ctx, key); err != nil {
		if errors.Is(err, jetstream.ErrKeyNotFound) {
			return ErrAddonNotFound
		}
		return fmt.Errorf("kv get %s: %w", key, err)
	}
	if err := kv.Delete(ctx, key); err != nil {
		return fmt.Errorf("kv delete %s: %w", key, err)
	}
	// Best-effort: drop the staged manifest so a re-create starts clean.
	manifestKey := AddonManifestKey(cluster, addon)
	if err := kv.Delete(ctx, manifestKey); err != nil && !errors.Is(err, jetstream.ErrKeyNotFound) {
		return fmt.Errorf("kv delete %s: %w", manifestKey, err)
	}
	return nil
}

// casUpdateAddon does a revision-checked read-modify-write.
// mutate returns true when a field changed. Returns ErrAddonNotFound if absent.
func casUpdateAddon(ctx context.Context, kv jetstream.KeyValue, cluster, addon string, mutate func(*AddonRecord) bool) (*AddonRecord, error) {
	key := AddonKey(cluster, addon)
	for range maxClusterStateCASRetries {
		entry, err := kv.Get(ctx, key)
		if err != nil {
			if errors.Is(err, jetstream.ErrKeyNotFound) {
				return nil, ErrAddonNotFound
			}
			return nil, fmt.Errorf("kv get %s: %w", key, err)
		}
		var rec AddonRecord
		if err := json.Unmarshal(entry.Value(), &rec); err != nil {
			return nil, fmt.Errorf("unmarshal addon %s: %w", addon, err)
		}
		if !mutate(&rec) {
			return &rec, nil
		}
		data, err := json.Marshal(&rec)
		if err != nil {
			return nil, fmt.Errorf("marshal addon %s: %w", addon, err)
		}
		_, err = kv.Update(ctx, key, data, entry.Revision())
		if err == nil {
			return &rec, nil
		}
		if errors.Is(err, jetstream.ErrKeyExists) {
			continue
		}
		return nil, fmt.Errorf("kv update %s: %w", key, err)
	}
	return nil, fmt.Errorf("eks: casUpdateAddon %s exhausted CAS retries", addon)
}

// sortAddonRecords orders records by add-on name. One record exists per add-on
// name, so the ordering is total and an unstable sort suffices.
func sortAddonRecords(recs []*AddonRecord) {
	slices.SortFunc(recs, func(a, b *AddonRecord) int {
		return strings.Compare(a.AddonName, b.AddonName)
	})
}
