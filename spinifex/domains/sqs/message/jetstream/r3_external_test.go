package jetstream_test

import (
	"context"
	"os"
	"os/exec"
	"testing"
	"time"

	"github.com/mulgadc/spinifex/spinifex/domains/sqs/message"
	"github.com/mulgadc/spinifex/spinifex/domains/sqs/message/conformance"
	sqsjs "github.com/mulgadc/spinifex/spinifex/domains/sqs/message/jetstream"
	"github.com/mulgadc/spinifex/spinifex/foundation/state/clustersize"
	"github.com/nats-io/nats.go"
	jsapi "github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// External multi-host R3 run. SQS_STORE_R3_URLS names a dedicated three-node
// SQS JetStream cluster, never the platform's; SQS_STORE_R3_RESTART, when
// set, is a shell command that kills and restarts every node, for Reopen.
const (
	externalURLsEnv    = "SQS_STORE_R3_URLS"
	externalRestartEnv = "SQS_STORE_R3_RESTART"
)

func connectExternal(t *testing.T, urls string) jsapi.JetStream {
	t.Helper()
	nc, err := nats.Connect(urls, nats.MaxReconnects(-1))
	require.NoError(t, err)
	t.Cleanup(nc.Close)
	js, err := jsapi.New(nc)
	require.NoError(t, err)
	require.Eventually(t, func() bool {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_, err := js.AccountInfo(ctx)
		return err == nil
	}, 60*time.Second, 500*time.Millisecond, "JetStream meta leader not elected")
	require.Eventually(t, func() bool { return allStreamsLed(js) }, 120*time.Second, 500*time.Millisecond, "stream leaders not elected")
	return js
}

func externalStore(t *testing.T, js jsapi.JetStream) *sqsjs.Store {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	s, err := sqsjs.New(ctx, js, testConfig)
	require.NoError(t, err)
	return s
}

func TestR3ExternalConformance(t *testing.T) {
	urls := os.Getenv(externalURLsEnv)
	if urls == "" {
		t.Skipf("set %s to a dedicated three-node SQS JetStream cluster", externalURLsEnv)
	}
	restart := os.Getenv(externalRestartEnv)
	clustersize.RedeclareForTest(t, 3)
	js := connectExternal(t, urls)

	externalStore(t, js)
	ctx := context.Background()
	for i := range testConfig.MessageStreams {
		stream, err := js.Stream(ctx, sqsjs.StreamName(message.Shard(i)))
		require.NoError(t, err)
		info := stream.CachedInfo()
		assert.Equal(t, 3, info.Config.Replicas, "stream %d", i)
		require.NotNil(t, info.Cluster)
		assert.Len(t, info.Cluster.Replicas, 2, "stream %d has a leader and two peers", i)
		t.Logf("%s leader %s peers %d", info.Config.Name, info.Cluster.Leader, len(info.Cluster.Replicas))
	}

	conformance.Run(t, func(t *testing.T) conformance.Harness {
		h := conformance.Harness{
			Store:         externalStore(t, js),
			MessageShards: testConfig.MessageStreams,
			StateShards:   testConfig.StateBuckets,
		}
		if restart != "" {
			h.Reopen = func(t *testing.T) message.Store {
				out, err := exec.Command("sh", "-c", restart).CombinedOutput()
				require.NoError(t, err, "restart: %s", out)
				js = connectExternal(t, urls)
				return externalStore(t, js)
			}
		}
		return h
	})
}
