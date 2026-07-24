package handlers_eks

import (
	"context"
	"errors"
	"fmt"

	"github.com/nats-io/nats.go/jetstream"
)

// claimKey atomically creates key with payload. owned is true when this caller
// wins the CAS-create. On an existing key it returns owned=false with the
// current value + revision; a key that vanished between Create and Get returns
// owned=false, existing=nil so the caller can retry.
// This is the shared idempotency primitive: the first caller wins and launches;
// duplicates (SDK retries, re-published handlers) lose and never launch.
func claimKey(ctx context.Context, kv jetstream.KeyValue, key string, payload []byte) (owned bool, existing []byte, rev uint64, err error) {
	if _, cerr := kv.Create(ctx, key, payload); cerr == nil {
		return true, nil, 0, nil
	} else if !errors.Is(cerr, jetstream.ErrKeyExists) {
		return false, nil, 0, fmt.Errorf("kv create %s: %w", key, cerr)
	}
	entry, gerr := kv.Get(ctx, key)
	if gerr != nil {
		if errors.Is(gerr, jetstream.ErrKeyNotFound) {
			return false, nil, 0, nil
		}
		return false, nil, 0, fmt.Errorf("kv get %s: %w", key, gerr)
	}
	return false, entry.Value(), entry.Revision(), nil
}
