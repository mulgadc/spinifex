package awsgw

import (
	"context"
	"encoding/json"
	"strconv"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	handlers_quota "github.com/mulgadc/spinifex/spinifex/handlers/quota"
	"github.com/mulgadc/spinifex/spinifex/testutil"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/require"
)

// respondJSON subscribes to subject and replies with out to every request, so
// the reconcile describe fan-out and KV bucket queries resolve without daemons.
func respondJSON(t *testing.T, nc *nats.Conn, subject string, out *ec2.DescribeInstancesOutput) {
	t.Helper()
	data, err := json.Marshal(out)
	require.NoError(t, err)
	sub, err := nc.Subscribe(subject, func(msg *nats.Msg) {
		_ = msg.Respond(data)
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = sub.Unsubscribe() })
}

// TestRunQuotaReconcile drives the leader-locked reconcile loop end to end: the
// startup pass elects this gateway, describes the account's instances over NATS
// via the production NATSInstanceLister, and writes the recomputed vCPU counter.
func TestRunQuotaReconcile(t *testing.T) {
	_, nc, _ := testutil.StartTestJetStream(t)
	bucket, err := openAccountUsageBucket(t.Context(), testutil.NewJetStream(t, nc), 1)
	require.NoError(t, err)
	quota := handlers_quota.New(handlers_quota.Limits{Enabled: true, VCPUs: 100}, bucket)

	const account = "123456789012"
	// Running fan-out reports one m5.xlarge (4 vCPUs); the KV buckets are empty.
	respondJSON(t, nc, "ec2.DescribeInstances", &ec2.DescribeInstancesOutput{
		Reservations: []*ec2.Reservation{{Instances: []*ec2.Instance{{
			InstanceType: aws.String("m5.xlarge"),
			State:        &ec2.InstanceState{Name: aws.String(ec2.InstanceStateNameRunning)},
		}}}},
	})
	respondJSON(t, nc, "ec2.DescribeStoppedInstances", &ec2.DescribeInstancesOutput{})
	respondJSON(t, nc, "ec2.DescribeTerminatedInstances", &ec2.DescribeInstancesOutput{})

	accounts := func() ([]string, error) { return []string{account}, nil }
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		runQuotaReconcile(ctx, quota, nc, accounts, func() int { return 1 })
		close(done)
	}()

	require.Eventually(t, func() bool {
		return readUsageCounter(t, bucket, account) == 4
	}, 5*time.Second, 20*time.Millisecond, "counter should reconcile to 4 vCPUs")

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("runQuotaReconcile did not return after ctx cancel")
	}
}

// readUsageCounter reads an account's integer vCPU counter from the usage bucket,
// returning -1 while the key is still absent so Eventually keeps polling.
func readUsageCounter(t *testing.T, bucket jetstream.KeyValue, account string) int {
	t.Helper()
	entry, err := bucket.Get(t.Context(), account)
	if err != nil {
		return -1
	}
	n, err := strconv.Atoi(string(entry.Value()))
	require.NoError(t, err)
	return n
}
