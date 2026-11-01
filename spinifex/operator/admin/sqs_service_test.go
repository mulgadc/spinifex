package admin

import (
	"context"
	"errors"
	"testing"
	"time"

	testutil "github.com/mulgadc/spinifex/internal/testkit"
	"github.com/mulgadc/spinifex/spinifex/domains/sqs/servicestate"
	"github.com/nats-io/nats.go/jetstream"
)

func enableInput() SQSEnableInput {
	return SQSEnableInput{Region: "ap-southeast-2", NodeID: "node1", WorkerInstanceType: "t3.small",
		StoreInstanceType: "t3.small", VolumeSizeGiB: "8", AccountID: "000000000000"}
}

// Outside a single-node cluster, enable is refused before any state exists.
func TestEnableSQS_RefusesAMultiNodeClusterWritingNothing(t *testing.T) {
	_, nc, js := testutil.StartTestJetStream(t)
	ctx := context.Background()
	for _, tc := range []struct {
		nodes    []string
		expected int
	}{{[]string{"node1", "node2", "node3"}, 3}, {[]string{"node1"}, 3}, {[]string{"node1", "node2"}, 1}} {
		if _, err := EnableSQS(ctx, nc, js, tc.nodes, tc.expected, enableInput(), nil, time.Second); err == nil {
			t.Fatalf("enable on %v/%d succeeded", tc.nodes, tc.expected)
		}
	}
	if _, err := js.KeyValue(ctx, servicestate.Bucket); !errors.Is(err, jetstream.ErrBucketNotFound) {
		t.Fatalf("service state bucket = %v; want none created", err)
	}
}

// An enable whose launches fail stays recorded as desired, and status shows
// it as requested but not ready rather than as disabled or done.
func TestEnableSQS_InterruptedEnableStaysRequestedAndNotReady(t *testing.T) {
	_, nc, js := testutil.StartTestJetStream(t)
	ctx := context.Background()
	before, err := DescribeSQS(ctx, js, "ap-southeast-2", nil)
	if err != nil || before.Desired != "disabled" || before.Store != sqsStoreAbsent || before.Ready {
		t.Fatalf("status before enable = %+v, %v", before, err)
	}
	if _, err := EnableSQS(ctx, nc, js, []string{"node1"}, 1, enableInput(), nil, time.Second); err == nil {
		t.Fatal("enable with no node daemon answering succeeded")
	}
	after, err := DescribeSQS(ctx, js, "ap-southeast-2", func() bool { return true })
	if err != nil || after.Desired != "enabled" || after.NodeID != "node1" || after.Store != sqsStoreAbsent || after.Ready {
		t.Fatalf("status after interrupted enable = %+v, %v", after, err)
	}
	in := enableInput()
	in.NodeID = "node2"
	if _, err := EnableSQS(ctx, nc, js, []string{"node2"}, 1, in, nil, time.Second); err == nil {
		t.Fatal("enable on another node succeeded while node1 holds the service")
	}
}
