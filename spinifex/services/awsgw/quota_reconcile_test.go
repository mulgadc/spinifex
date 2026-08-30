package awsgw

import (
	"context"
	"encoding/json"
	"strconv"
	"testing"
	"time"

	handlers_quota "github.com/mulgadc/spinifex/spinifex/handlers/quota"
	"github.com/mulgadc/spinifex/spinifex/resource"
	"github.com/mulgadc/spinifex/spinifex/testutil"
	"github.com/mulgadc/spinifex/spinifex/vm"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/require"
)

func TestOpenAccountUsageBucketPreservesExistingConfiguration(t *testing.T) {
	_, nc, _ := testutil.StartTestJetStream(t)
	js := testutil.NewJetStream(t, nc)
	_, err := js.CreateKeyValue(t.Context(), jetstream.KeyValueConfig{
		Bucket:  handlers_quota.KVBucketAccountUsage,
		History: 5,
	})
	require.NoError(t, err)

	bucket, err := openAccountUsageBucket(t.Context(), js, 1)
	require.NoError(t, err)
	status, err := bucket.Status(t.Context())
	require.NoError(t, err)
	require.EqualValues(t, 5, status.History())
}

// putInstanceRecord writes one instance record at the key the sweep reads, so a
// test can state an account's holdings without a node or a describe responder.
func putInstanceRecord(t *testing.T, records jetstream.KeyValue, id, accountID, instanceType string, state vm.InstanceState) {
	t.Helper()
	data, err := json.Marshal(vm.InstanceRecord{
		Metadata: resource.Metadata{Name: id, AccountID: accountID},
		Spec:     vm.InstanceSpec{InstanceType: instanceType},
		Status:   vm.InstanceStatus{Status: state},
	})
	require.NoError(t, err)
	_, err = records.Put(t.Context(), "i."+id, data)
	require.NoError(t, err)
}

// instanceRecordBucket creates the shared instance-state bucket the sweep reads
// and the loop watches.
func instanceRecordBucket(t *testing.T, js jetstream.JetStream) jetstream.KeyValue {
	t.Helper()
	kv, err := js.CreateKeyValue(t.Context(), jetstream.KeyValueConfig{
		Bucket:  "spinifex-instance-state",
		History: 1,
	})
	require.NoError(t, err)
	return kv
}

// The startup pass recomputes the counter from the instance record space. No
// describe responder is registered anywhere in this test: if the loop still
// fanned out over NATS it would have nothing to fan out to.
func TestRunQuotaReconcileCountsFromTheRecordSpace(t *testing.T) {
	_, nc, _ := testutil.StartTestJetStream(t)
	js := testutil.NewJetStream(t, nc)
	bucket, err := openAccountUsageBucket(t.Context(), js, 1)
	require.NoError(t, err)
	quota := handlers_quota.New(handlers_quota.Limits{Enabled: true, VCPUs: 100}, bucket)

	const account = "123456789012"
	records := instanceRecordBucket(t, js)
	putInstanceRecord(t, records, "i-aaa", account, "m5.xlarge", vm.StateRunning) // 4
	putInstanceRecord(t, records, "i-bbb", account, "t3.micro", vm.StateStopped)  // 2
	// Terminal instances no longer exist for quota.
	putInstanceRecord(t, records, "i-ccc", account, "m5.xlarge", vm.StateTerminated)

	accounts := func() ([]string, error) { return []string{account}, nil }
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		runQuotaReconcile(ctx, quota, nc, js, accounts, 1)
		close(done)
	}()

	require.Eventually(t, func() bool {
		return readUsageCounter(t, bucket, account) == 6
	}, 10*time.Second, 20*time.Millisecond, "counter should reconcile to 6 vCPUs from the records")

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("runQuotaReconcile did not return after ctx cancel")
	}
}

// A launch is a record write, and the loop is woken by it rather than by a
// tick: the counter moves well inside the resync interval.
func TestRunQuotaReconcileFollowsARecordChange(t *testing.T) {
	_, nc, _ := testutil.StartTestJetStream(t)
	js := testutil.NewJetStream(t, nc)
	bucket, err := openAccountUsageBucket(t.Context(), js, 1)
	require.NoError(t, err)
	quota := handlers_quota.New(handlers_quota.Limits{Enabled: true, VCPUs: 100}, bucket)

	const account = "123456789012"
	records := instanceRecordBucket(t, js)
	putInstanceRecord(t, records, "i-aaa", account, "m5.xlarge", vm.StateRunning)

	accounts := func() ([]string, error) { return []string{account}, nil }
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go runQuotaReconcile(ctx, quota, nc, js, accounts, 1)

	require.Eventually(t, func() bool {
		return readUsageCounter(t, bucket, account) == 4
	}, 10*time.Second, 20*time.Millisecond, "startup pass should settle at 4")

	putInstanceRecord(t, records, "i-bbb", account, "t3.micro", vm.StateRunning)
	require.Eventually(t, func() bool {
		return readUsageCounter(t, bucket, account) == 6
	}, 10*time.Second, 20*time.Millisecond, "the record write should wake the loop and raise to 6")

	// A terminate removes the record. The account cannot be read off a
	// tombstone, so this is the whole-set fallback rather than a per-key pass.
	require.NoError(t, records.Delete(t.Context(), "i.i-bbb"))
	require.Eventually(t, func() bool {
		return readUsageCounter(t, bucket, account) == 4
	}, 10*time.Second, 20*time.Millisecond, "the delete should lower the counter back to 4")
}

// A record change recomputes only the account that owns it. The drifted counter
// on the other account is what proves it: a whole-set pass would correct it too,
// and the resync that eventually will is a full interval away.
func TestRunQuotaReconcileRecomputesOnlyTheChangedAccount(t *testing.T) {
	_, nc, _ := testutil.StartTestJetStream(t)
	js := testutil.NewJetStream(t, nc)
	bucket, err := openAccountUsageBucket(t.Context(), js, 1)
	require.NoError(t, err)
	quota := handlers_quota.New(handlers_quota.Limits{Enabled: true, VCPUs: 100}, bucket)

	const changed, untouched = "111111111111", "222222222222"
	records := instanceRecordBucket(t, js)
	putInstanceRecord(t, records, "i-aaa", changed, "m5.xlarge", vm.StateRunning)

	accounts := func() ([]string, error) { return []string{changed, untouched}, nil }
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go runQuotaReconcile(ctx, quota, nc, js, accounts, 1)

	require.Eventually(t, func() bool {
		return readUsageCounter(t, bucket, changed) == 4
	}, 10*time.Second, 20*time.Millisecond, "startup pass should settle the first account")

	// Drift the other account after the startup pass has been and gone.
	require.NoError(t, quota.AddVCPU(t.Context(), untouched, 32))

	putInstanceRecord(t, records, "i-bbb", changed, "t3.micro", vm.StateRunning)
	require.Eventually(t, func() bool {
		return readUsageCounter(t, bucket, changed) == 6
	}, 10*time.Second, 20*time.Millisecond, "the changed account should recompute to 6")

	require.Equal(t, 32, readUsageCounter(t, bucket, untouched),
		"a change to one account must not recompute another; the resync is what eventually corrects it")
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
