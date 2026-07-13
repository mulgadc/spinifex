package utils

import (
	"testing"

	"github.com/mulgadc/spinifex/spinifex/testutil"
)

func TestDefaultKVReplicas(t *testing.T) {
	t.Cleanup(func() { defaultKVReplicas.Store(0) })

	if got := DefaultKVReplicas(); got != 1 {
		t.Fatalf("unset default = %d, want 1", got)
	}
	SetDefaultKVReplicas(0)
	if got := DefaultKVReplicas(); got != 1 {
		t.Fatalf("SetDefaultKVReplicas(0) = %d, want 1 (clamped)", got)
	}
	SetDefaultKVReplicas(3)
	if got := DefaultKVReplicas(); got != 3 {
		t.Fatalf("SetDefaultKVReplicas(3) = %d, want 3", got)
	}
}

func TestGetOrCreateKVBucketUsesDefaultReplicas(t *testing.T) {
	t.Cleanup(func() { defaultKVReplicas.Store(0) })
	_, _, js := testutil.StartTestJetStream(t)

	// A single-node test server can only host R1, so assert the default is
	// threaded into the created stream config rather than the hardcoded 1.
	// The R>1 case is exercised on a live multi-node cluster.
	SetDefaultKVReplicas(1)
	if _, err := GetOrCreateKVBucket(js, "test-default-replicas", 1); err != nil {
		t.Fatalf("GetOrCreateKVBucket: %v", err)
	}
	info, err := js.StreamInfo("KV_test-default-replicas")
	if err != nil {
		t.Fatalf("StreamInfo: %v", err)
	}
	if info.Config.Replicas != 1 {
		t.Fatalf("bucket replicas = %d, want 1", info.Config.Replicas)
	}
}
