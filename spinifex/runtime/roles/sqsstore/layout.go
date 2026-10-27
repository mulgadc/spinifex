package sqsstore

import (
	"time"

	"github.com/nats-io/nats.go/jetstream"
)

// MessageRetentionBackstop outlasts the longest queue retention (14 days) so
// the store never expires a message before the domain does.
const MessageRetentionBackstop = 14*24*time.Hour + time.Hour

// ShardCounts is the number of shared message streams and state buckets.
type ShardCounts struct {
	Message int
	State   int
}

// DefaultShardCounts returns the shard counts for a topology of the given size.
func DefaultShardCounts(nodes int) ShardCounts {
	if nodes >= 3 {
		return ShardCounts{Message: 8, State: 8}
	}
	return ShardCounts{Message: 16, State: 8}
}

// Replicas returns the JetStream replica count for a topology: R1 on one
// node, R3 from three.
func Replicas(nodes int) int {
	if nodes >= 3 {
		return 3
	}
	return 1
}

// MessageStreamConfig returns the config of one shared message stream.
// Persistence stays at the default mode so acknowledgements follow a sync.
func MessageStreamConfig(name string, subjects []string, replicas int) jetstream.StreamConfig {
	return jetstream.StreamConfig{
		Name:        name,
		Subjects:    subjects,
		Retention:   jetstream.LimitsPolicy,
		Storage:     jetstream.FileStorage,
		Replicas:    replicas,
		MaxAge:      MessageRetentionBackstop,
		PersistMode: jetstream.DefaultPersistMode,
	}
}
