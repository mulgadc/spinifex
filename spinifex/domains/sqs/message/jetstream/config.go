// Package jetstream implements the SQS message store on the dedicated NATS
// JetStream server: a fixed set of shared message streams with a subject per
// queue, and a fixed set of shared state buckets (ADR-0011:S9 to S12).
package jetstream

import (
	"fmt"
	"strconv"
	"time"

	"github.com/mulgadc/spinifex/spinifex/domains/sqs/message"
	jsapi "github.com/nats-io/nats.go/jetstream"
)

// DefaultDuplicateWindow is how long a stream remembers a Nats-Msg-Id, which
// is the window an idempotent append holds for.
const DefaultDuplicateWindow = 2 * time.Minute

// Config sizes the shared layout new queues are allocated from. Counts are
// per topology (ADR-0011:S10) and may change; a queue's recorded allocation
// does not. Replicas follow the cluster size: R1 on one node, R3 on three.
type Config struct {
	MessageStreams  int
	StateBuckets    int
	DuplicateWindow time.Duration
}

// DefaultConfig is ADR-0011:S10's default for a topology of nodes: 16
// message streams and 8 state buckets on one node, 8 and 8 from three.
func DefaultConfig(nodes int) Config {
	cfg := Config{MessageStreams: 16, StateBuckets: 8, DuplicateWindow: DefaultDuplicateWindow}
	if nodes >= 3 {
		cfg.MessageStreams = 8
	}
	return cfg
}

// Validate rejects a layout the adapter could not serve.
func (c Config) Validate() error {
	switch {
	case c.MessageStreams < 1:
		return fmt.Errorf("%w: %d message streams", message.ErrInvalidArgument, c.MessageStreams)
	case c.StateBuckets < 1:
		return fmt.Errorf("%w: %d state buckets", message.ErrInvalidArgument, c.StateBuckets)
	case c.DuplicateWindow <= 0 || c.DuplicateWindow > message.MinRetentionBackstop:
		return fmt.Errorf("%w: duplicate window %s outside (0, %s]", message.ErrInvalidArgument, c.DuplicateWindow, message.MinRetentionBackstop)
	}
	return nil
}

// StreamName is the shared message stream serving a shard.
func StreamName(shard message.Shard) string {
	return "SQS_M_" + strconv.FormatUint(uint64(shard), 10)
}

// BucketName is the shared state bucket serving a state shard.
func BucketName(shard message.Shard) string {
	return "sqs_state_" + strconv.FormatUint(uint64(shard), 10)
}

// Subject is where a namespace's messages are published:
// sqs.m.<message shard>.<queue>.
func Subject(ns message.Namespace) string {
	return "sqs.m." + strconv.FormatUint(uint64(ns.MessageShard), 10) + "." + string(ns.Queue)
}

// StateKey is a message's state key in the bucket named by its StateShard.
// A position is unique only within a message stream, so the key carries the
// message shard too.
func StateKey(ns message.Namespace, pos message.Position) string {
	return "q." + strconv.FormatUint(uint64(ns.MessageShard), 10) + "." + string(ns.Queue) + "." + strconv.FormatUint(uint64(pos), 10)
}

// MessageStreamConfig is one shared message stream: limits retention, the
// ADR-0011:S12 backstop as MaxAge, and the default persist mode so an
// acknowledgement follows a sync (ADR-0011:S3).
func (c Config) MessageStreamConfig(shard message.Shard, replicas int) jsapi.StreamConfig {
	return jsapi.StreamConfig{
		Name:        StreamName(shard),
		Description: "SQS messages, shared by every queue assigned this shard",
		Subjects:    []string{"sqs.m." + strconv.FormatUint(uint64(shard), 10) + ".>"},
		Retention:   jsapi.LimitsPolicy,
		Storage:     jsapi.FileStorage,
		Replicas:    replicas,
		MaxAge:      message.MinRetentionBackstop,
		Duplicates:  c.DuplicateWindow,
		PersistMode: jsapi.DefaultPersistMode,
	}
}
