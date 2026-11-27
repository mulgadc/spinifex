package elbv2

import (
	"context"
	"errors"
	"time"

	"github.com/mulgadc/spinifex/spinifex/foundation/state/kvstore"
	"github.com/nats-io/nats.go/jetstream"
)

// kvOperationHeader marks a KV stream message that deletes or purges its key.
const kvOperationHeader = "KV-Operation"

// fixtureLeaderGet point-reads key as the stream leader holds it. An absent,
// deleted or purged key is jetstream.ErrKeyNotFound; any other failure is an error.
type fixtureLeaderGet func(ctx context.Context, key string) (jetstream.KeyValueEntry, error)

// leaderGetOf reads bucket's keys by one stream message get, which only the
// stream leader answers, so a lagging replica's older value is never seen.
func leaderGetOf(js jetstream.JetStream, bucket string) fixtureLeaderGet {
	return func(ctx context.Context, key string) (jetstream.KeyValueEntry, error) {
		msg, err := kvstore.LeaderMsgGet(ctx, js, "KV_"+bucket, kvstore.MsgGetRequest{LastFor: "$KV." + bucket + "." + key})
		if errors.Is(err, jetstream.ErrMsgNotFound) {
			return nil, jetstream.ErrKeyNotFound
		}
		if err != nil {
			return nil, err
		}
		if op := msg.Header.Get(kvOperationHeader); op == "DEL" || op == "PURGE" {
			return nil, jetstream.ErrKeyNotFound
		}
		return &leaderEntry{bucket: bucket, key: key, value: msg.Data, revision: msg.Sequence, created: msg.Time}, nil
	}
}

// leaderEntry is a live KV value read from the stream leader.
type leaderEntry struct {
	bucket, key string
	value       []byte
	revision    uint64
	created     time.Time
}

var _ jetstream.KeyValueEntry = (*leaderEntry)(nil)

func (e *leaderEntry) Bucket() string                  { return e.bucket }
func (e *leaderEntry) Key() string                     { return e.key }
func (e *leaderEntry) Value() []byte                   { return e.value }
func (e *leaderEntry) Revision() uint64                { return e.revision }
func (e *leaderEntry) Created() time.Time              { return e.created }
func (e *leaderEntry) Delta() uint64                   { return 0 }
func (e *leaderEntry) Operation() jetstream.KeyValueOp { return jetstream.KeyValuePut }
