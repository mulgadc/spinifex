package sqsstore

import (
	"testing"
	"time"

	natsserver "github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func validOpts() *natsserver.Options {
	return &natsserver.Options{JetStream: true, SyncAlways: true, MaxPayload: MinMaxPayload}
}

func TestValidateOptions(t *testing.T) {
	require.NoError(t, ValidateOptions(validOpts()))

	cases := map[string]func(*natsserver.Options){
		"jetstream off":     func(o *natsserver.Options) { o.JetStream = false },
		"periodic sync":     func(o *natsserver.Options) { o.SyncAlways = false },
		"control-plane cap": func(o *natsserver.Options) { o.MaxPayload = 1024 * 1024 },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			o := validOpts()
			mutate(o)
			assert.Error(t, ValidateOptions(o))
		})
	}
}

func TestNewRequiresConfigFile(t *testing.T) {
	_, err := New(&Config{})
	assert.Error(t, err)
	_, err = New("not a config")
	assert.Error(t, err)
	svc, err := New(&Config{ConfigFile: "/x"})
	require.NoError(t, err)
	assert.NotNil(t, svc)
}

func TestMessageStreamConfigNeverAsyncPersist(t *testing.T) {
	for _, replicas := range []int{Replicas(1), Replicas(3)} {
		cfg := MessageStreamConfig("sqs_msg_0", []string{"sqs.msg.0.>"}, replicas)
		assert.Equal(t, jetstream.DefaultPersistMode, cfg.PersistMode)
		assert.Equal(t, jetstream.LimitsPolicy, cfg.Retention)
		assert.Equal(t, jetstream.FileStorage, cfg.Storage)
		assert.Equal(t, 14*24*time.Hour+time.Hour, cfg.MaxAge)
		assert.Equal(t, replicas, cfg.Replicas)
	}
}

func TestShardCountsPerTopology(t *testing.T) {
	assert.Equal(t, ShardCounts{Message: 16, State: 8}, DefaultShardCounts(1))
	assert.Equal(t, ShardCounts{Message: 8, State: 8}, DefaultShardCounts(3))
	assert.Equal(t, 1, Replicas(1))
	assert.Equal(t, 3, Replicas(3))
}
