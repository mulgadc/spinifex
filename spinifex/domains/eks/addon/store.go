package addon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/nats-io/nats.go/jetstream"
)

// casRetries bounds casUpdate's revision-checked retry loop. Matches the
// shared EKS cluster-state CAS retry count.
const casRetries = 5

// manifestSuffix names the staged-manifest sub-key under a record key.
const manifestSuffix = "/manifest"

// Prefix returns the KV key prefix under which all of a cluster's managed
// add-on records live.
func Prefix(cluster string) string {
	return fmt.Sprintf("clusters/%s/addons/", cluster)
}

// Key returns the KV key for a managed add-on record under a cluster.
func Key(cluster, addon string) string {
	return Prefix(cluster) + addon
}

// ManifestKey returns the KV key under which an add-on's manifest descriptor
// is staged for the guest delivery transport to consume.
func ManifestKey(cluster, addon string) string {
	return Prefix(cluster) + addon + manifestSuffix
}

// put writes the record unconditionally.
func put(ctx context.Context, kv jetstream.KeyValue, cluster string, rec *Record) error {
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
	key := Key(cluster, rec.AddonName)
	if _, err := kv.Put(ctx, key, data); err != nil {
		return fmt.Errorf("kv put %s: %w", key, err)
	}
	return nil
}

// Get reads one record. Returns ErrNotFound if absent.
func Get(ctx context.Context, kv jetstream.KeyValue, cluster, addon string) (*Record, error) {
	if cluster == "" || addon == "" {
		return nil, errors.New("eks: GetAddonRecord empty cluster or addon name")
	}
	entry, err := kv.Get(ctx, Key(cluster, addon))
	if err != nil {
		if errors.Is(err, jetstream.ErrKeyNotFound) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("kv get addon: %w", err)
	}
	var rec Record
	if err := json.Unmarshal(entry.Value(), &rec); err != nil {
		return nil, fmt.Errorf("unmarshal addon %s: %w", addon, err)
	}
	return &rec, nil
}

// List returns all add-on records under a cluster, sorted by name.
// Staged-manifest sub-keys (one extra path segment) are skipped.
func List(ctx context.Context, kv jetstream.KeyValue, cluster string) ([]*Record, error) {
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
	prefix := Prefix(cluster)
	out := make([]*Record, 0)
	for _, k := range keys {
		rest, ok := strings.CutPrefix(k, prefix)
		if !ok {
			continue
		}
		// Record keys are one segment under the prefix; deeper keys are sub-keys.
		if strings.Contains(rest, "/") {
			continue
		}
		entry, err := kv.Get(ctx, k)
		if err != nil {
			if errors.Is(err, jetstream.ErrKeyNotFound) {
				continue
			}
			return nil, fmt.Errorf("kv get %s: %w", k, err)
		}
		var rec Record
		if err := json.Unmarshal(entry.Value(), &rec); err != nil {
			return nil, fmt.Errorf("unmarshal addon %s: %w", k, err)
		}
		out = append(out, &rec)
	}
	sortRecords(out)
	return out, nil
}

// ListManifests returns every manifest staged for a cluster, sorted by add-on
// name. An empty bucket yields an empty, non-nil slice.
func ListManifests(ctx context.Context, kv jetstream.KeyValue, cluster string) ([]Manifest, error) {
	keys, err := kv.Keys(ctx)
	if err != nil {
		if errors.Is(err, jetstream.ErrNoKeysFound) {
			return []Manifest{}, nil
		}
		return nil, err
	}
	prefix := Prefix(cluster)
	out := make([]Manifest, 0)
	for _, k := range keys {
		if !strings.HasPrefix(k, prefix) || !strings.HasSuffix(k, manifestSuffix) {
			continue
		}
		entry, err := kv.Get(ctx, k)
		if err != nil {
			if errors.Is(err, jetstream.ErrKeyNotFound) {
				continue
			}
			return nil, err
		}
		var m Manifest
		if err := json.Unmarshal(entry.Value(), &m); err != nil {
			return nil, fmt.Errorf("unmarshal staged manifest %s: %w", k, err)
		}
		out = append(out, m)
	}
	slices.SortFunc(out, func(a, b Manifest) int {
		return strings.Compare(a.AddonName, b.AddonName)
	})
	return out, nil
}

// putManifest stages the record's delivery descriptor unconditionally.
func putManifest(ctx context.Context, kv jetstream.KeyValue, cluster string, rec *Record) error {
	manifest := Manifest{
		AddonName:             rec.AddonName,
		AddonVersion:          rec.AddonVersion,
		ServiceAccountRoleArn: rec.ServiceAccountRoleArn,
		ConfigurationValues:   rec.ConfigurationValues,
	}
	data, err := json.Marshal(&manifest)
	if err != nil {
		return fmt.Errorf("marshal staged manifest %s: %w", rec.AddonName, err)
	}
	key := ManifestKey(cluster, rec.AddonName)
	if _, err := kv.Put(ctx, key, data); err != nil {
		return fmt.Errorf("kv put %s: %w", key, err)
	}
	return nil
}

// deleteManifest removes a staged manifest; an absent one is not an error.
func deleteManifest(ctx context.Context, kv jetstream.KeyValue, cluster, addon string) error {
	key := ManifestKey(cluster, addon)
	if err := kv.Delete(ctx, key); err != nil && !errors.Is(err, jetstream.ErrKeyNotFound) {
		return fmt.Errorf("kv delete %s: %w", key, err)
	}
	return nil
}

// deleteRecord removes one record and its staged manifest. Returns ErrNotFound if absent.
func deleteRecord(ctx context.Context, kv jetstream.KeyValue, cluster, addon string) error {
	key := Key(cluster, addon)
	if _, err := kv.Get(ctx, key); err != nil {
		if errors.Is(err, jetstream.ErrKeyNotFound) {
			return ErrNotFound
		}
		return fmt.Errorf("kv get %s: %w", key, err)
	}
	if err := kv.Delete(ctx, key); err != nil {
		return fmt.Errorf("kv delete %s: %w", key, err)
	}
	// Drop the staged manifest too so a re-create starts clean.
	return deleteManifest(ctx, kv, cluster, addon)
}

// casUpdate does a revision-checked read-modify-write. mutate returns true
// when a field changed. Returns ErrNotFound if absent.
func casUpdate(ctx context.Context, kv jetstream.KeyValue, cluster, addon string, mutate func(*Record) bool) (*Record, error) {
	key := Key(cluster, addon)
	for range casRetries {
		entry, err := kv.Get(ctx, key)
		if err != nil {
			if errors.Is(err, jetstream.ErrKeyNotFound) {
				return nil, ErrNotFound
			}
			return nil, fmt.Errorf("kv get %s: %w", key, err)
		}
		var rec Record
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
		if errors.Is(err, jetstream.ErrKeyRevisionMismatch) {
			continue
		}
		return nil, fmt.Errorf("kv update %s: %w", key, err)
	}
	return nil, fmt.Errorf("eks: casUpdateAddon %s exhausted CAS retries", addon)
}

// sortRecords orders records by add-on name. One record exists per add-on
// name, so the ordering is total and an unstable sort suffices.
func sortRecords(recs []*Record) {
	slices.SortFunc(recs, func(a, b *Record) int {
		return strings.Compare(a.AddonName, b.AddonName)
	})
}
