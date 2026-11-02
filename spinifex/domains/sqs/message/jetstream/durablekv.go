package jetstream

import (
	"context"
	"errors"
	"fmt"

	"github.com/mulgadc/spinifex/spinifex/foundation/state/clustersize"
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
	return verifyDurable(ctx, kv, bucket)
}

// BindDurableKV binds an existing SQS authority bucket without ever creating
// one, so a reader that must not provision state fails closed when it is absent,
// and refuses it on the same durability terms as OpenDurableKV.
func BindDurableKV(ctx context.Context, js jsapi.JetStream, bucket string) (jsapi.KeyValue, error) {
	kv, err := js.KeyValue(ctx, bucket)
	if err != nil {
		return nil, fmt.Errorf("bind bucket %s: %w", bucket, err)
	}
	return verifyDurable(ctx, kv, bucket)
}

func verifyDurable(ctx context.Context, kv jsapi.KeyValue, bucket string) (jsapi.KeyValue, error) {
	replicas, err := clustersize.Replicas()
	if err != nil {
		return nil, fmt.Errorf("bucket %s: %w", bucket, err)
	}
	status, err := kv.Status(ctx)
	if err != nil {
		return nil, fmt.Errorf("read bucket %s configuration: %w", bucket, err)
	}
	st, ok := status.(*jsapi.KeyValueBucketStatus)
	if !ok || st.StreamInfo() == nil {
		return nil, fmt.Errorf("bucket %s: stream configuration unavailable", bucket)
	}
	if err := checkDurable(st.StreamInfo().Config, replicas); err != nil {
		return nil, fmt.Errorf("bucket %s is not durable: %w", bucket, err)
	}
	return kv, nil
}

// checkDurable refuses any effective stream setting under which an accepted
// authority write could later expire, be evicted or lost, lag a source, or be
// refused; the bucket's records are compared and swapped, never re-derived.
func checkDurable(sc jsapi.StreamConfig, replicas int) error {
	switch {
	case sc.Retention != jsapi.LimitsPolicy:
		return fmt.Errorf("retention is %s, not limits", sc.Retention)
	case sc.MaxAge != 0:
		return fmt.Errorf("records expire after %s", sc.MaxAge)
	case sc.AllowMsgTTL:
		return errors.New("per-message TTLs can expire records")
	case sc.SubjectDeleteMarkerTTL != 0:
		return errors.New("delete markers expire")
	case sc.Storage != jsapi.FileStorage:
		return errors.New("storage is not file-backed")
	case sc.PersistMode != jsapi.DefaultPersistMode:
		return errors.New("writes are flushed asynchronously")
	case sc.MaxBytes > 0 || sc.MaxMsgs > 0 || sc.MaxMsgSize > 0:
		return errors.New("a size or message cap can evict or refuse records")
	case sc.MaxMsgsPerSubject < 1:
		return errors.New("per-key history is unbounded or zero")
	case sc.Discard != jsapi.DiscardNew || sc.DiscardNewPerSubject:
		return errors.New("the discard policy can evict or refuse records")
	case sc.NoAck:
		return errors.New("writes are not acknowledged")
	case sc.Sealed:
		return errors.New("the bucket is sealed")
	case sc.Mirror != nil || len(sc.Sources) > 0:
		return errors.New("the bucket mirrors or sources another stream")
	case sc.Replicas < replicas:
		return fmt.Errorf("%d replicas, want at least %d", sc.Replicas, replicas)
	}
	return nil
}
