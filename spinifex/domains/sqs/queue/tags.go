package queue

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"strings"
	"unicode/utf8"
)

// Tag limits from the SQS queue quotas page; the model declares none, so the
// limits and the error they map to are provisional until characterised.
const (
	maxTags        = 50
	maxTagKeyLen   = 128
	maxTagValueLen = 256
	reservedPrefix = "aws:"
)

// ErrInvalidTag rejects a tag request; its wire code is provisional.
var ErrInvalidTag = errors.New("invalid tag")

// ValidateTags checks tags supplied to CreateQueue or TagQueue. A nil or
// empty map is valid only when allowEmpty, as on CreateQueue.
func ValidateTags(tags map[string]string, allowEmpty bool) error {
	if len(tags) == 0 && !allowEmpty {
		return fmt.Errorf("%w: at least one tag is required", ErrInvalidTag)
	}
	if len(tags) > maxTags {
		return fmt.Errorf("%w: at most %d tags", ErrInvalidTag, maxTags)
	}
	for _, k := range sortedKeys(tags) {
		if err := validTagKey(k); err != nil {
			return err
		}
		v := tags[k]
		if !utf8.ValidString(v) || utf8.RuneCountInString(v) > maxTagValueLen {
			return fmt.Errorf("%w: value of %q must be at most %d characters", ErrInvalidTag, k, maxTagValueLen)
		}
	}
	return nil
}

// ValidateTagKeys checks the keys supplied to UntagQueue.
func ValidateTagKeys(keys []string) error {
	if len(keys) == 0 {
		return fmt.Errorf("%w: at least one tag key is required", ErrInvalidTag)
	}
	for _, k := range keys {
		if err := validTagKey(k); err != nil {
			return err
		}
	}
	return nil
}

func validTagKey(k string) error {
	n := utf8.RuneCountInString(k)
	switch {
	case !utf8.ValidString(k) || n == 0 || n > maxTagKeyLen:
		return fmt.Errorf("%w: key %q must be 1 to %d characters", ErrInvalidTag, k, maxTagKeyLen)
	case strings.HasPrefix(strings.ToLower(k), reservedPrefix):
		return fmt.Errorf("%w: key %q uses the reserved %q prefix", ErrInvalidTag, k, reservedPrefix)
	}
	return nil
}

// Tag adds or overwrites tags on the named queue all or nothing, advancing
// its generation on every accepted call but not its attribute modification
// time.
func (s *Store) Tag(ctx context.Context, account, region, name string, tags map[string]string) (Record, error) {
	if err := ValidateTags(tags, false); err != nil {
		return Record{}, err
	}
	return s.update(ctx, account, region, name, change{
		op: "TagQueue",
		apply: func(r *Record) (Record, error) {
			next := *r
			next.Tags = maps.Clone(r.Tags)
			if next.Tags == nil {
				next.Tags = make(map[string]string, len(tags))
			}
			maps.Copy(next.Tags, tags)
			if len(next.Tags) > maxTags {
				return Record{}, fmt.Errorf("%w: queue would have %d tags, more than %d", ErrInvalidTag, len(next.Tags), maxTags)
			}
			return next, nil
		},
	})
}

// Untag removes the keys from the named queue, advancing its generation on
// every accepted call; keys the queue does not carry are ignored.
func (s *Store) Untag(ctx context.Context, account, region, name string, keys []string) (Record, error) {
	if err := ValidateTagKeys(keys); err != nil {
		return Record{}, err
	}
	return s.update(ctx, account, region, name, change{
		op: "UntagQueue",
		apply: func(r *Record) (Record, error) {
			next := *r
			next.Tags = maps.Clone(r.Tags)
			for _, k := range keys {
				delete(next.Tags, k)
			}
			return next, nil
		},
	})
}

// Tags returns the named active queue's tags, never nil.
func (s *Store) Tags(ctx context.Context, account, region, name string) (map[string]string, error) {
	r, _, err := s.resolve(ctx, account, region, name)
	if err != nil {
		return nil, err
	}
	if err := settable(&r); err != nil {
		return nil, err
	}
	out := maps.Clone(r.Tags)
	if out == nil {
		out = map[string]string{}
	}
	return out, nil
}
