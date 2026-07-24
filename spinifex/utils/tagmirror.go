package utils

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"

	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	"github.com/nats-io/nats.go/jetstream"
)

// MergeTagsMut returns a mutator that merges the CreateTagsInput tags into a
// record's tag map. Tags with a nil key or value are skipped.
func MergeTagsMut(input *ec2.CreateTagsInput) func(map[string]string) {
	return func(tags map[string]string) {
		for _, t := range input.Tags {
			if t.Key != nil && t.Value != nil {
				tags[*t.Key] = *t.Value
			}
		}
	}
}

// ApplyTagRemovals applies AWS-faithful DeleteTags semantics to a tag map:
// keys delete unconditionally, valueMatch entries delete only when the stored
// value matches, and both empty clears every tag.
func ApplyTagRemovals(tags map[string]string, keys []string, valueMatch map[string]string) {
	if len(keys) == 0 && len(valueMatch) == 0 {
		clear(tags)
		return
	}
	for _, k := range keys {
		delete(tags, k)
	}
	for k, v := range valueMatch {
		if cur, ok := tags[k]; ok && cur == v {
			delete(tags, k)
		}
	}
}

// RemoveTagsMut returns a mutator with AWS-faithful DeleteTags semantics:
// empty input.Tags clears all tags; a tag with a value deletes only on a
// value match, a nil value deletes unconditionally.
func RemoveTagsMut(input *ec2.DeleteTagsInput) func(map[string]string) {
	return func(tags map[string]string) {
		if len(input.Tags) == 0 {
			ApplyTagRemovals(tags, nil, nil)
			return
		}
		var keys []string
		var valueMatch map[string]string
		for _, t := range input.Tags {
			if t.Key == nil {
				continue
			}
			if t.Value == nil {
				keys = append(keys, *t.Key)
			} else {
				if valueMatch == nil {
					valueMatch = make(map[string]string)
				}
				valueMatch[*t.Key] = *t.Value
			}
		}
		// All entries had nil keys: no removals requested, not a clear-all.
		if len(keys) == 0 && len(valueMatch) == 0 {
			return
		}
		ApplyTagRemovals(tags, keys, valueMatch)
	}
}

// UpdateKVRecordTags read-modify-writes a typed account-scoped KV record,
// applying mut. A resource absent from this store is skipped (its tags live
// elsewhere).
func UpdateKVRecordTags[R any](ctx context.Context, kv jetstream.KeyValue, accountID, resourceID string, mut func(*R)) error {
	key := AccountKey(accountID, resourceID)
	entry, err := kv.Get(ctx, key)
	if err != nil {
		if errors.Is(err, jetstream.ErrKeyNotFound) {
			return nil
		}
		slog.Error("UpdateKVRecordTags: KV read failed", "key", key, "err", err)
		return errors.New(awserrors.ErrorServerInternal)
	}
	var rec R
	if err := json.Unmarshal(entry.Value(), &rec); err != nil {
		slog.Error("UpdateKVRecordTags: record unmarshal failed", "key", key, "err", err)
		return errors.New(awserrors.ErrorServerInternal)
	}
	mut(&rec)
	data, err := json.Marshal(&rec)
	if err != nil {
		slog.Error("UpdateKVRecordTags: record marshal failed", "key", key, "err", err)
		return errors.New(awserrors.ErrorServerInternal)
	}
	if _, err := kv.Put(ctx, key, data); err != nil {
		slog.Error("UpdateKVRecordTags: KV write failed", "key", key, "err", err)
		return errors.New(awserrors.ErrorServerInternal)
	}
	return nil
}

// MirrorKVRecordTags applies mut to the tag map of every resource in
// resources carrying prefix, via UpdateKVRecordTags on the service's own KV
// handle. tagsOf returns the address of the record's tag map so a nil map can
// be initialized in place. Resource ids without prefix are skipped.
func MirrorKVRecordTags[R any](ctx context.Context, kv jetstream.KeyValue, accountID, prefix string, resources []*string, tagsOf func(*R) *map[string]string, mut func(map[string]string)) error {
	for _, res := range resources {
		if res == nil || !strings.HasPrefix(*res, prefix) {
			continue
		}
		if err := UpdateKVRecordTags(ctx, kv, accountID, *res, func(r *R) {
			tp := tagsOf(r)
			if *tp == nil {
				*tp = map[string]string{}
			}
			mut(*tp)
		}); err != nil {
			return err
		}
	}
	return nil
}
