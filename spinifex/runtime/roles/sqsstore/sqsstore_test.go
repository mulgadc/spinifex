package sqsstore

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime/debug"
	"testing"
	"time"

	natsserver "github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
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

func writeConf(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "sqs-store.conf")
	require.NoError(t, os.WriteFile(p, []byte(body), 0o600))
	return p
}

func storeConf(t *testing.T, syncLine string, payload string) string {
	t.Helper()
	return writeConf(t, `
server_name: test-sqs-store
listen: 127.0.0.1:-1
jetstream {
  `+syncLine+`
  store_dir: "`+t.TempDir()+`"
}
max_payload: `+payload+`
`)
}

func TestStartServerRefusesInvalidConfig(t *testing.T) {
	cases := map[string]string{
		"missing file":    filepath.Join(t.TempDir(), "absent.conf"),
		"periodic sync":   storeConf(t, "sync_interval: 1s", "2MB"),
		"small payload":   storeConf(t, "sync_interval: always", "1MB"),
		"jetstream unset": writeConf(t, "listen: 127.0.0.1:-1\nmax_payload: 2MB\n"),
	}
	for name, path := range cases {
		t.Run(name, func(t *testing.T) {
			ns, _, err := startServer(&Config{ConfigFile: path})
			require.Error(t, err)
			assert.Nil(t, ns)
		})
	}
}

func TestStartServerAcceptsFullSizeMessage(t *testing.T) {
	ns, stop, err := startServer(&Config{ConfigFile: storeConf(t, "sync_interval: always", "2MB")})
	require.NoError(t, err)
	defer ns.Shutdown()
	defer stop()

	nc, err := nats.Connect(ns.ClientURL())
	require.NoError(t, err)
	defer nc.Close()
	js, err := nc.JetStream()
	require.NoError(t, err)
	_, err = js.AddStream(&nats.StreamConfig{Name: "Q", Subjects: []string{"q.>"}})
	require.NoError(t, err)

	msg := nats.NewMsg("q.one")
	msg.Data = make([]byte, 1<<20)
	for i := range 10 {
		msg.Header.Set(fmt.Sprintf("attr%d", i), "v")
	}
	ack, err := js.PublishMsg(msg)
	require.NoError(t, err)
	assert.Equal(t, uint64(1), ack.Sequence)
}

func TestApplyMemoryLimit(t *testing.T) {
	prev := debug.SetMemoryLimit(-1)
	defer debug.SetMemoryLimit(prev)

	t.Setenv("GOMEMLIMIT", "1GiB")
	debug.SetMemoryLimit(12345)
	applyMemoryLimit()
	assert.Equal(t, int64(12345), debug.SetMemoryLimit(-1), "an operator-set limit is left alone")

	t.Setenv("GOMEMLIMIT", "")
	applyMemoryLimit()
	assert.Equal(t, int64(MemoryLimitBytes), debug.SetMemoryLimit(-1))
}

func TestStartFailsOnInvalidConfigAfterWritingPidFile(t *testing.T) {
	dir := t.TempDir()
	svc, err := New(&Config{ConfigFile: storeConf(t, "sync_interval: 1s", "2MB"), DataDir: dir})
	require.NoError(t, err)

	pid, err := svc.Start()
	require.Error(t, err)
	assert.Zero(t, pid)
	assert.FileExists(t, filepath.Join(dir, "sqs-store.pid"))
}
