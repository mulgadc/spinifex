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

func checkDurable(sc jsapi.StreamConfig, replicas int) error {
	switch {
	case sc.MaxAge != 0:
		return fmt.Errorf("records expire after %s", sc.MaxAge)
	case sc.Storage != jsapi.FileStorage:
		return errors.New("storage is not file-backed")
	case sc.MaxBytes > 0 || sc.MaxMsgs > 0:
		return errors.New("a size or message cap can evict records")
	case sc.MaxMsgsPerSubject < 1:
		return errors.New("per-key history is unbounded or zero")
	case sc.SubjectDeleteMarkerTTL != 0:
		return errors.New("delete markers expire")
	case sc.Replicas < replicas:
		return fmt.Errorf("%d replicas, want at least %d", sc.Replicas, replicas)
	}
	return nil
}
