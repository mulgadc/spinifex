package utils

import (
	"encoding/json"
	"errors"
	"strings"

	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	"github.com/nats-io/nats.go"
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

// RemoveTagsMut returns a mutator with AWS-faithful DeleteTags semantics:
// empty input.Tags clears all tags; a tag with a value deletes only on a
// value match, a nil value deletes unconditionally.
func RemoveTagsMut(input *ec2.DeleteTagsInput) func(map[string]string) {
	return func(tags map[string]string) {
		if len(input.Tags) == 0 {
			for k := range tags {
				delete(tags, k)
			}
			return
		}
		for _, t := range input.Tags {
			if t.Key == nil {
				continue
			}
			if t.Value == nil {
				delete(tags, *t.Key)
			} else if cur, ok := tags[*t.Key]; ok && cur == *t.Value {
				delete(tags, *t.Key)
			}
		}
	}
}

// UpdateKVRecordTags read-modify-writes a typed account-scoped KV record,
// applying mut. A resource absent from this store is skipped (its tags live
// elsewhere).
func UpdateKVRecordTags[R any](kv nats.KeyValue, accountID, resourceID string, mut func(*R)) error {
	key := AccountKey(accountID, resourceID)
	entry, err := kv.Get(key)
	if err != nil {
		if errors.Is(err, nats.ErrKeyNotFound) {
			return nil
		}
		return errors.New(awserrors.ErrorServerInternal)
	}
	var rec R
	if err := json.Unmarshal(entry.Value(), &rec); err != nil {
		return errors.New(awserrors.ErrorServerInternal)
	}
	mut(&rec)
	data, err := json.Marshal(&rec)
	if err != nil {
		return errors.New(awserrors.ErrorServerInternal)
	}
	if _, err := kv.Put(key, data); err != nil {
		return errors.New(awserrors.ErrorServerInternal)
	}
	return nil
}

// MirrorKVRecordTags applies mut to the tag map of every resource in
// resources carrying prefix, via UpdateKVRecordTags on the service's own KV
// handle. tagsOf returns the address of the record's tag map so a nil map can
// be initialized in place. Resource ids without prefix are skipped.
func MirrorKVRecordTags[R any](kv nats.KeyValue, accountID, prefix string, resources []*string, tagsOf func(*R) *map[string]string, mut func(map[string]string)) error {
	for _, res := range resources {
		if res == nil || !strings.HasPrefix(*res, prefix) {
			continue
		}
		if err := UpdateKVRecordTags(kv, accountID, *res, func(r *R) {
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
