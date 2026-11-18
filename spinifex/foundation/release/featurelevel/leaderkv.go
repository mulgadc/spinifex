package featurelevel

import (
	"context"
	"errors"
	"time"

	"github.com/mulgadc/spinifex/spinifex/foundation/state/kvstore"
	"github.com/nats-io/nats.go/jetstream"
)

// leaderKV reads through the stream leader so a lagging replica never returns
// the record from before a level was raised or a rollback began.
type leaderKV struct {
	jetstream.KeyValue

	js jetstream.JetStream
}

var _ KV = (*leaderKV)(nil)

// LeaderKV returns kv with Get answered by the stream leader. Writes are
// unchanged: the leader already serialises them.
func LeaderKV(js jetstream.JetStream, kv jetstream.KeyValue) KV {
	return &leaderKV{KeyValue: kv, js: js}
}

func (k *leaderKV) Get(ctx context.Context, key string) (jetstream.KeyValueEntry, error) {
	bucket := k.Bucket()
	msg, err := kvstore.LeaderMsgGet(ctx, k.js, "KV_"+bucket, kvstore.MsgGetRequest{LastFor: "$KV." + bucket + "." + key})
	if errors.Is(err, jetstream.ErrMsgNotFound) {
		return nil, jetstream.ErrKeyNotFound
	}
	if err != nil {
		return nil, err
	}
	if op := msg.Header.Get("KV-Operation"); op == "DEL" || op == "PURGE" {
		return nil, jetstream.ErrKeyNotFound
	}
	return &leaderEntry{bucket: bucket, key: key, msg: msg}, nil
}

// leaderEntry is the value and revision of one leader read.
type leaderEntry struct {
	bucket, key string
	msg         *jetstream.RawStreamMsg
}

func (e *leaderEntry) Bucket() string                  { return e.bucket }
func (e *leaderEntry) Key() string                     { return e.key }
func (e *leaderEntry) Value() []byte                   { return e.msg.Data }
func (e *leaderEntry) Revision() uint64                { return e.msg.Sequence }
func (e *leaderEntry) Created() time.Time              { return e.msg.Time }
func (e *leaderEntry) Delta() uint64                   { return 0 }
func (e *leaderEntry) Operation() jetstream.KeyValueOp { return jetstream.KeyValuePut }
