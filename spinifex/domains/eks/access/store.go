package access

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/nats-io/nats.go/jetstream"
)

// casRetries bounds CASUpdate's revision-checked retry loop. Matches the
// shared EKS cluster-state CAS retry count (maxClusterStateCASRetries).
const casRetries = 5

// ErrNotFound is returned when no entry exists for the principal. Callers
// translate it to ResourceNotFoundException at the service boundary.
var ErrNotFound = errors.New("eks: access entry not found")

// Prefix returns the KV key prefix under which all of a cluster's AccessEntry
// records live.
func Prefix(cluster string) string {
	return fmt.Sprintf("clusters/%s/access-entries/", cluster)
}

// Key returns the KV key for an AccessEntry record under a cluster. The
// principal ARN is hashed because IAM ARNs contain ':' which is not a legal
// NATS JetStream KV key character; the record itself carries the plaintext ARN.
func Key(cluster, principalARN string) string {
	return Prefix(cluster) + PrincipalARNHash(principalARN)
}

// PrincipalARNHash maps an IAM principal ARN to a KV-key-safe token.
func PrincipalARNHash(principalARN string) string {
	sum := sha256.Sum256([]byte(principalARN))
	return hex.EncodeToString(sum[:])
}

// put writes the entry unconditionally.
func put(ctx context.Context, kv jetstream.KeyValue, rec *Record) error {
	if rec == nil {
		return errors.New("eks: PutAccessEntryRecord nil record")
	}
	if rec.ClusterName == "" || rec.PrincipalARN == "" {
		return errors.New("eks: PutAccessEntryRecord missing cluster or principal ARN")
	}
	data, err := json.Marshal(rec)
	if err != nil {
		return fmt.Errorf("marshal access entry %s: %w", rec.PrincipalARN, err)
	}
	key := Key(rec.ClusterName, rec.PrincipalARN)
	if _, err := kv.Put(ctx, key, data); err != nil {
		return fmt.Errorf("kv put %s: %w", key, err)
	}
	return nil
}

// Get reads one entry. Returns ErrNotFound if absent.
func Get(ctx context.Context, kv jetstream.KeyValue, cluster, principalARN string) (*Record, error) {
	if cluster == "" || principalARN == "" {
		return nil, errors.New("eks: GetAccessEntryRecord empty cluster or principal ARN")
	}
	entry, err := kv.Get(ctx, Key(cluster, principalARN))
	if err != nil {
		if errors.Is(err, jetstream.ErrKeyNotFound) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("kv get access entry: %w", err)
	}
	var rec Record
	if err := json.Unmarshal(entry.Value(), &rec); err != nil {
		return nil, fmt.Errorf("unmarshal access entry %s: %w", principalARN, err)
	}
	return &rec, nil
}

// List returns all access entries under a cluster, sorted by principal ARN.
func List(ctx context.Context, kv jetstream.KeyValue, cluster string) ([]*Record, error) {
	if cluster == "" {
		return nil, errors.New("eks: ListAccessEntryRecords empty cluster")
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
		if !strings.HasPrefix(k, prefix) {
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
			return nil, fmt.Errorf("unmarshal access entry %s: %w", k, err)
		}
		out = append(out, &rec)
	}
	sortRecords(out)
	return out, nil
}

// deleteRecord removes one entry; returns ErrNotFound if absent.
func deleteRecord(ctx context.Context, kv jetstream.KeyValue, cluster, principalARN string) error {
	key := Key(cluster, principalARN)
	if _, err := kv.Get(ctx, key); err != nil {
		if errors.Is(err, jetstream.ErrKeyNotFound) {
			return ErrNotFound
		}
		return fmt.Errorf("kv get %s: %w", key, err)
	}
	if err := kv.Delete(ctx, key); err != nil {
		return fmt.Errorf("kv delete %s: %w", key, err)
	}
	return nil
}

// casUpdate does a revision-checked read-modify-write. mutate returns true
// when a field changed. Returns ErrNotFound if absent.
func casUpdate(ctx context.Context, kv jetstream.KeyValue, cluster, principalARN string, mutate func(*Record) bool) (*Record, error) {
	key := Key(cluster, principalARN)
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
			return nil, fmt.Errorf("unmarshal access entry %s: %w", principalARN, err)
		}
		if !mutate(&rec) {
			return &rec, nil
		}
		data, err := json.Marshal(&rec)
		if err != nil {
			return nil, fmt.Errorf("marshal access entry %s: %w", principalARN, err)
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
	return nil, fmt.Errorf("eks: casUpdateAccessEntry %s exhausted CAS retries", principalARN)
}

// sortRecords orders entries by principal ARN. One record exists per
// principal, so the ordering is total and an unstable sort suffices.
func sortRecords(recs []*Record) {
	slices.SortFunc(recs, func(a, b *Record) int {
		return strings.Compare(a.PrincipalARN, b.PrincipalARN)
	})
}
