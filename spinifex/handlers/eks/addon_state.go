package handlers_eks

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/nats-io/nats.go"
)

// AddonStatus mirrors the AWS EKS addon.status enum verbatim so clients match
// against the values without translation.
type AddonStatus string

const (
	AddonStatusCreating     AddonStatus = "CREATING"
	AddonStatusActive       AddonStatus = "ACTIVE"
	AddonStatusUpdating     AddonStatus = "UPDATING"
	AddonStatusDeleting     AddonStatus = "DELETING"
	AddonStatusDegraded     AddonStatus = "DEGRADED"
	AddonStatusCreateFailed AddonStatus = "CREATE_FAILED"
)

// AddonRecord is the persisted-state envelope for a managed add-on bound to a
// cluster. It tracks the AWS-visible lifecycle status plus the IRSA role and
// opaque configuration the installer needs.
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

// ErrAddonNotFound is returned by GetAddonRecord / the CAS helper when no
// record exists for the add-on. Callers translate to the AWS shape
// (ResourceNotFoundException) at the service boundary.
var ErrAddonNotFound = errors.New("eks: addon not found")

// AddonARN composes the deterministic ARN for a managed add-on (one per
// add-on name per cluster).
func AddonARN(region, accountID, cluster, addon string) string {
	return fmt.Sprintf("arn:aws:eks:%s:%s:addon/%s/%s", region, accountID, cluster, addon)
}

// PutAddonRecord writes the record unconditionally.
func PutAddonRecord(kv nats.KeyValue, cluster string, rec *AddonRecord) error {
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
	if _, err := kv.Put(key, data); err != nil {
		return fmt.Errorf("kv put %s: %w", key, err)
	}
	return nil
}

// GetAddonRecord reads one record. Returns ErrAddonNotFound if absent.
func GetAddonRecord(kv nats.KeyValue, cluster, addon string) (*AddonRecord, error) {
	if cluster == "" || addon == "" {
		return nil, errors.New("eks: GetAddonRecord empty cluster or addon name")
	}
	entry, err := kv.Get(AddonKey(cluster, addon))
	if err != nil {
		if errors.Is(err, nats.ErrKeyNotFound) {
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

// ListAddonRecords returns every add-on record under a cluster, sorted by add-on
// name for stable output. The staged-manifest sub-keys
// (clusters/{c}/addons/{addon}/manifest) are skipped — only the record keys
// (one path segment under the prefix) are returned.
func ListAddonRecords(kv nats.KeyValue, cluster string) ([]*AddonRecord, error) {
	if cluster == "" {
		return nil, errors.New("eks: ListAddonRecords empty cluster")
	}
	keys, err := kv.Keys()
	if err != nil {
		if errors.Is(err, nats.ErrNoKeysFound) {
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
		// Skip sub-keys (e.g. the staged manifest); a record key is exactly one
		// segment under the prefix.
		if strings.Contains(strings.TrimPrefix(k, prefix), "/") {
			continue
		}
		entry, err := kv.Get(k)
		if err != nil {
			if errors.Is(err, nats.ErrKeyNotFound) {
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

// DeleteAddonRecord removes one record (and its staged manifest, if any).
// Returns ErrAddonNotFound if the record did not exist.
func DeleteAddonRecord(kv nats.KeyValue, cluster, addon string) error {
	key := AddonKey(cluster, addon)
	if _, err := kv.Get(key); err != nil {
		if errors.Is(err, nats.ErrKeyNotFound) {
			return ErrAddonNotFound
		}
		return fmt.Errorf("kv get %s: %w", key, err)
	}
	if err := kv.Delete(key); err != nil {
		return fmt.Errorf("kv delete %s: %w", key, err)
	}
	// Best-effort: drop the staged manifest so a re-create starts clean.
	manifestKey := AddonManifestKey(cluster, addon)
	if err := kv.Delete(manifestKey); err != nil && !errors.Is(err, nats.ErrKeyNotFound) {
		return fmt.Errorf("kv delete %s: %w", manifestKey, err)
	}
	return nil
}

// casUpdateAddon does a revision-checked read-modify-write. mutate returns true
// if it changed a field. Returns ErrAddonNotFound if absent.
func casUpdateAddon(kv nats.KeyValue, cluster, addon string, mutate func(*AddonRecord) bool) (*AddonRecord, error) {
	key := AddonKey(cluster, addon)
	for range maxClusterStateCASRetries {
		entry, err := kv.Get(key)
		if err != nil {
			if errors.Is(err, nats.ErrKeyNotFound) {
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
		_, err = kv.Update(key, data, entry.Revision())
		if err == nil {
			return &rec, nil
		}
		if errors.Is(err, nats.ErrKeyExists) {
			continue
		}
		return nil, fmt.Errorf("kv update %s: %w", key, err)
	}
	return nil, fmt.Errorf("eks: casUpdateAddon %s exhausted CAS retries", addon)
}

func sortAddonRecords(recs []*AddonRecord) {
	for i := 1; i < len(recs); i++ {
		for j := i; j > 0 && recs[j-1].AddonName > recs[j].AddonName; j-- {
			recs[j-1], recs[j] = recs[j], recs[j-1]
		}
	}
}
