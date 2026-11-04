package jetstream

import (
	"context"

	"github.com/mulgadc/spinifex/spinifex/foundation/state/kvutil"
	jsapi "github.com/nats-io/nats.go/jetstream"
)

// OpenDurableKV opens an SQS authority bucket through the shared chokepoint,
// creating it with one value per key and no TTL when absent, and refuses one
// whose stored configuration could expire, cap or evict records, or holds
// fewer replicas than the cluster needs.
func OpenDurableKV(ctx context.Context, js jsapi.JetStream, bucket, description string) (jsapi.KeyValue, error) {
	kv, err := kvutil.GetOrCreateBucketWithOptions(ctx, js, kvutil.BucketOptions{
		Name: bucket, Description: description, History: 1,
	})
	if err != nil {
		return nil, err
	}
	return kvutil.VerifyDurable(ctx, kv, bucket)
}

// BindDurableKV binds an existing SQS authority bucket without ever creating
// one, so a reader that must not provision state fails closed when it is absent,
// and refuses it on the same durability terms as OpenDurableKV.
func BindDurableKV(ctx context.Context, js jsapi.JetStream, bucket string) (jsapi.KeyValue, error) {
	return kvutil.BindDurable(ctx, js, bucket)
}
