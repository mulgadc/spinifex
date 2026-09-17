package handlers_ec2_tags

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/mulgadc/spinifex/spinifex/kvutil"
	"github.com/nats-io/nats.go/jetstream"
)

// KVBucketTags holds one entry per tagged resource, keyed {accountID}.{resourceID}.
// The entry is the resource's whole tag map, so every mutation is a read-modify-
// write and has to be committed at the revision it was read at.
const KVBucketTags = "ec2-tags"

// kvTagsHistory is the bucket's history depth. One is enough: nothing reads a
// superseded tag map, and a deeper history only slows the CAS retries down.
const kvTagsHistory = 1

// tagCASAttempts is the retry budget for one tag mutation, set well above the
// shared default because a Terraform apply tags one resource from several
// parallel calls and a writer can lose the race repeatedly. Spending the budget
// returns an error the caller retries, which is the right failure for this
// store: a slow tag write is recoverable and a lost one is not.
const tagCASAttempts = 12

// tagsKVKey is the bucket key for a resource. The dot is JetStream KV's own
// subject separator, and neither half can contain one, so the two parts stay
// recoverable from the key.
func tagsKVKey(accountID, resourceID string) string {
	return accountID + "." + resourceID
}

// resourceIDFromKVKey recovers the resource ID from a bucket key, reporting
// false for a key belonging to another account or carrying no resource at all.
func resourceIDFromKVKey(key, accountID string) (string, bool) {
	rest, found := strings.CutPrefix(key, accountID+".")
	if !found || rest == "" {
		return "", false
	}
	return rest, true
}

// GetOrCreateTagsBucket opens the tag bucket, creating it at the cluster's
// replica count if this is the first node to reach it.
func GetOrCreateTagsBucket(ctx context.Context, js jetstream.KeyValueManager) (jetstream.KeyValue, error) {
	return kvutil.GetOrCreateBucket(ctx, js, KVBucketTags, kvTagsHistory)
}

// readKVTags returns a resource's tags from the bucket, and whether the key
// exists at all. An absent key is not an error: it means the resource has never
// been written here, and the caller falls back to the legacy object.
func (s *TagsServiceImpl) readKVTags(ctx context.Context, accountID, resourceID string) (map[string]string, bool, error) {
	entry, err := s.kv.Get(ctx, tagsKVKey(accountID, resourceID))
	if err != nil {
		if errors.Is(err, jetstream.ErrKeyNotFound) {
			return nil, false, nil
		}
		return nil, false, err
	}
	tags, err := decodeTags(entry.Value())
	if err != nil {
		return nil, false, err
	}
	return tags, true, nil
}

// mutateTags applies mutate to a resource's tag map under optimistic
// concurrency, so writers on different nodes cannot lose each other's keys.
//
// A resource with no KV entry yet is seeded from its legacy S3 object inside the
// attempt rather than before it, so a migration failure fails this one call
// instead of quietly committing a map that has dropped everything written
// before the store moved.
func (s *TagsServiceImpl) mutateTags(ctx context.Context, accountID, resourceID string, mutate func(map[string]string)) error {
	key := tagsKVKey(accountID, resourceID)
	_, err := kvutil.Update(ctx, s.kv, key, kvutil.CASConfig{
		Attempts:       tagCASAttempts,
		CreateIfAbsent: true,
		Exhausted: func(key string, attempts int) error {
			return fmt.Errorf("tag store contended for %s after %d attempts", key, attempts)
		},
	}, func(tags *map[string]string) (bool, error) {
		if *tags == nil {
			seeded, serr := s.legacyTags(ctx, accountID, resourceID)
			if serr != nil {
				return false, serr
			}
			*tags = seeded
		}
		mutate(*tags)
		return true, nil
	})
	return err
}

// putTags replaces a resource's whole tag set. It still goes through the CAS
// loop so a concurrent merge is overwritten as a whole rather than interleaved
// with it, which is what a caller projecting an authoritative record wants.
func (s *TagsServiceImpl) putTags(ctx context.Context, accountID, resourceID string, tags map[string]string) error {
	data, err := encodeTags(tags)
	if err != nil {
		return err
	}
	return kvutil.Put(ctx, s.kv, tagsKVKey(accountID, resourceID), kvutil.CASConfig{
		Attempts:       tagCASAttempts,
		CreateIfAbsent: true,
	}, data)
}

// deleteTagsEntry removes a resource's bucket entry, treating an absent key as
// success so teardown stays idempotent.
func (s *TagsServiceImpl) deleteTagsEntry(ctx context.Context, accountID, resourceID string) error {
	err := s.kv.Delete(ctx, tagsKVKey(accountID, resourceID))
	if err != nil && !errors.Is(err, jetstream.ErrKeyNotFound) {
		return err
	}
	return nil
}
