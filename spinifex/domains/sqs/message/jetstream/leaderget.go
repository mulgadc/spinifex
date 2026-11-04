package jetstream

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/mulgadc/spinifex/spinifex/foundation/state/kvutil"
	"github.com/nats-io/nats.go"
	jsapi "github.com/nats-io/nats.go/jetstream"
)

// leaderLastMsg returns the last message on subject through a stream
// message get, which only the stream leader answers. nats.go switches to a
// direct get, which followers also answer, whenever the stream allows it.
func leaderLastMsg(ctx context.Context, js jsapi.JetStream, stream, subject string) (*jsapi.RawStreamMsg, error) {
	return kvutil.LeaderMsgGet(ctx, js, stream, kvutil.MsgGetRequest{LastFor: subject})
}

// LeaderKVGet reads key from an SQS KV bucket through the stream leader and
// returns its value and revision. An absent, deleted or purged key is
// jsapi.ErrKeyNotFound; any other failure is returned and decides nothing.
func LeaderKVGet(ctx context.Context, js jsapi.JetStream, bucket, key string) ([]byte, uint64, error) {
	m, err := leaderLastMsg(ctx, js, "KV_"+bucket, "$KV."+bucket+"."+key)
	switch {
	case errors.Is(err, jsapi.ErrMsgNotFound):
		return nil, 0, jsapi.ErrKeyNotFound
	case err != nil:
		return nil, 0, err
	case kvRemoved(m.Header):
		return nil, 0, jsapi.ErrKeyNotFound
	}
	return m.Data, m.Sequence, nil
}

// leaderStateKeys lists the live keys under keyPrefix in a state bucket up
// to sequence bound, with one leader message get per record.
func leaderStateKeys(ctx context.Context, js jsapi.JetStream, bucket, keyPrefix string, bound uint64) (map[string]struct{}, error) {
	stream, subjectPrefix := "KV_"+bucket, "$KV."+bucket+"."
	req := kvutil.MsgGetRequest{NextFor: subjectPrefix + keyPrefix + ">"}
	keys := map[string]struct{}{}
	for after := uint64(0); after < bound; {
		req.Seq = after + 1
		m, err := kvutil.LeaderMsgGet(ctx, js, stream, req)
		switch {
		case errors.Is(err, jsapi.ErrMsgNotFound):
			return keys, nil
		case err != nil:
			return nil, fmt.Errorf("read state bucket %s: %w", bucket, err)
		case m.Sequence <= after:
			return nil, fmt.Errorf("read state bucket %s: %w", bucket, jsapi.ErrInvalidJetStreamResponse)
		case m.Sequence > bound:
			return keys, nil
		}
		key := strings.TrimPrefix(m.Subject, subjectPrefix)
		if kvRemoved(m.Header) {
			delete(keys, key)
		} else {
			keys[key] = struct{}{}
		}
		after = m.Sequence
	}
	return keys, nil
}

// kvRemoved reports a key's delete or purge marker, as nats.go's KV Get
// reads one.
func kvRemoved(h nats.Header) bool {
	if op := h.Get("KV-Operation"); op != "" {
		return op == "DEL" || op == "PURGE"
	}
	switch h.Get(jsapi.MarkerReasonHeader) {
	case "MaxAge", "Purge", "Remove":
		return true
	}
	return false
}
