package handlers_eks

import (
	"errors"
	"fmt"

	"github.com/nats-io/nats.go"
)

// claimKey atomically creates key with payload. owned is true when this caller
// created it (won the claim). On an existing key it returns owned=false with the
// current value + revision so the caller can decide reclaim-vs-conflict; a key
// that vanished between the Create and the Get returns owned=false, existing=nil
// so the caller can retry the claim.
//
// This is the shared idempotency primitive for create handlers: the first
// caller to Create a resource's identity key wins and does the launching work;
// a duplicate (SDK retry, gateway re-publish that spawned a second handler)
// loses and never launches, so concurrent creates cannot double-launch + leak.
func claimKey(kv nats.KeyValue, key string, payload []byte) (owned bool, existing []byte, rev uint64, err error) {
	if _, cerr := kv.Create(key, payload); cerr == nil {
		return true, nil, 0, nil
	} else if !errors.Is(cerr, nats.ErrKeyExists) {
		return false, nil, 0, fmt.Errorf("kv create %s: %w", key, cerr)
	}
	entry, gerr := kv.Get(key)
	if gerr != nil {
		if errors.Is(gerr, nats.ErrKeyNotFound) {
			return false, nil, 0, nil
		}
		return false, nil, 0, fmt.Errorf("kv get %s: %w", key, gerr)
	}
	return false, entry.Value(), entry.Revision(), nil
}
