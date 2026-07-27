//go:build e2e

package harness

import (
	"testing"

	"github.com/mulgadc/spinifex/spinifex/utils"
	"github.com/nats-io/nats.go/jetstream"
)

// KVReplicaFactors connects to the cluster NATS and returns the configured
// replica factor for each named KV bucket, read from JetStream stream metadata
// (the "KV_" stream prefix is applied internally). The replica count lives in
// cluster-global Raft metadata, so any node's NATS reports the same value.
// Fatals if NATS is unreachable or a bucket's stream cannot be introspected.
func KVReplicaFactors(t *testing.T, env *Env, buckets ...string) map[string]int {
	t.Helper()
	host, token, ca := natsConn(t, env)
	nc, err := utils.ConnectNATS(host, token, ca)
	if err != nil {
		t.Fatalf("connect NATS %s: %v", host, err)
	}
	defer nc.Close()

	js, err := jetstream.New(nc)
	if err != nil {
		t.Fatalf("jetstream context: %v", err)
	}

	out := make(map[string]int, len(buckets))
	for _, b := range buckets {
		stream, err := js.Stream(t.Context(), "KV_"+b)
		if err != nil {
			t.Fatalf("open stream for KV bucket %q: %v", b, err)
		}
		info, err := stream.Info(t.Context())
		if err != nil {
			t.Fatalf("stream info for KV bucket %q: %v", b, err)
		}
		out[b] = info.Config.Replicas
	}
	return out
}
