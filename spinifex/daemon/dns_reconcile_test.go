package daemon

// These tests build *Daemon literals directly to reach the lazy watch-bucket
// listers, which is only possible from inside the package.
//test:in-package

import (
	"context"
	"testing"

	handlers_eks "github.com/mulgadc/spinifex/spinifex/handlers/eks"
	handlers_elbv2 "github.com/mulgadc/spinifex/spinifex/handlers/elbv2"
	handlers_iam "github.com/mulgadc/spinifex/spinifex/handlers/iam"
	handlers_rds "github.com/mulgadc/spinifex/spinifex/handlers/rds"
	"github.com/mulgadc/spinifex/spinifex/kvstore"
	"github.com/mulgadc/spinifex/spinifex/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestDNSWatchSources_OneSourcePerDesiredSetInput pins the filter each source
// watches on. The ELBv2 bucket also holds target groups, listeners, rules and
// name claims alongside load balancers, so its filter must stay the "lb."
// prefix rather than widen to ">" — a rule change is not a DNS change, and
// every write anywhere in that bucket would otherwise wake the reconcile.
func TestDNSWatchSources_OneSourcePerDesiredSetInput(t *testing.T) {
	d := &Daemon{}
	sources := d.dnsWatchSources()
	require.Len(t, sources, 4)

	filters := make([]string, 0, len(sources))
	for _, s := range sources {
		filters = append(filters, s.Filter())
	}
	assert.ElementsMatch(t, []string{
		InstanceStatePrefix + "*",
		handlers_elbv2.KeyPrefixLB + "*",
		">",
		">",
	}, filters)
}

// TestDNSWatchBuckets_ZeroValueDaemonIsNilSafe covers every lazy lister: the
// reconciler is constructed before the services it reads from exist, so each
// must degrade to (nil, nil) rather than panic on a daemon with nothing wired up.
func TestDNSWatchBuckets_ZeroValueDaemonIsNilSafe(t *testing.T) {
	d := &Daemon{}
	listers := map[string]func(context.Context) ([]*kvstore.Bucket, error){
		"instanceState": d.instanceStateWatchBuckets,
		"elbv2":         d.elbv2WatchBuckets,
		"eks":           d.eksWatchBuckets,
		"rds":           d.rdsWatchBuckets,
	}
	for name, list := range listers {
		t.Run(name, func(t *testing.T) {
			buckets, err := list(t.Context())
			require.NoError(t, err)
			assert.Nil(t, buckets)
		})
	}
}

func TestInstanceStateWatchBuckets_ReturnsSharedBucket(t *testing.T) {
	_, _, js := testutil.StartTestJetStream(t)
	d := &Daemon{jsManager: &JetStreamManager{js: js, replicas: 1}}

	buckets, err := d.instanceStateWatchBuckets(t.Context())
	require.NoError(t, err)
	require.Len(t, buckets, 1)
	assert.Equal(t, InstanceStateBucket, buckets[0].Name())
}

// TestElbv2WatchBuckets_NoStoreIsNilSafe covers a service that exists but has
// not opened a store, distinct from the elbv2Service field itself being nil.
func TestElbv2WatchBuckets_NoStoreIsNilSafe(t *testing.T) {
	d := &Daemon{elbv2Service: &handlers_elbv2.ELBv2ServiceImpl{}}
	buckets, err := d.elbv2WatchBuckets(t.Context())
	require.NoError(t, err)
	assert.Nil(t, buckets)
}

func TestElbv2WatchBuckets_WithStoreReturnsWatchBucket(t *testing.T) {
	_, nc, _ := testutil.StartTestJetStream(t)
	masterKey, err := handlers_iam.GenerateMasterKey()
	require.NoError(t, err)
	svc, err := handlers_elbv2.NewELBv2ServiceImplWithNATS(nil, nc, masterKey)
	require.NoError(t, err)
	t.Cleanup(svc.Close)

	d := &Daemon{elbv2Service: svc}
	buckets, err := d.elbv2WatchBuckets(t.Context())
	require.NoError(t, err)
	require.Len(t, buckets, 1)
	assert.Equal(t, handlers_elbv2.KVBucketELBv2, buckets[0].Name())
}

// TestEksWatchBuckets_NilGuards exercises the two halves of the guard clause
// independently: a natsConn with no service, and a service with no natsConn.
func TestEksWatchBuckets_NilGuards(t *testing.T) {
	_, nc, _ := testutil.StartTestJetStream(t)

	buckets, err := (&Daemon{natsConn: nc}).eksWatchBuckets(t.Context())
	require.NoError(t, err)
	assert.Nil(t, buckets)

	buckets, err = (&Daemon{eksService: &handlers_eks.EKSServiceImpl{}}).eksWatchBuckets(t.Context())
	require.NoError(t, err)
	assert.Nil(t, buckets)
}

func TestEksWatchBuckets_DelegatesToAccountWatchBuckets(t *testing.T) {
	_, nc, _ := testutil.StartTestJetStream(t)
	js := testutil.NewJetStream(t, nc)
	_, err := handlers_eks.GetOrCreateAccountBucket(t.Context(), js, "111111111111", 1)
	require.NoError(t, err)

	d := &Daemon{eksService: &handlers_eks.EKSServiceImpl{}, natsConn: nc}
	buckets, err := d.eksWatchBuckets(t.Context())
	require.NoError(t, err)
	require.Len(t, buckets, 1)
	assert.Equal(t, handlers_eks.AccountBucketName("111111111111"), buckets[0].Name())
}

// TestRdsWatchBuckets_NilGuards exercises the two halves of the guard clause
// independently: a jsManager with no service, and a service with no jsManager.
func TestRdsWatchBuckets_NilGuards(t *testing.T) {
	_, _, js := testutil.StartTestJetStream(t)

	buckets, err := (&Daemon{jsManager: &JetStreamManager{js: js}}).rdsWatchBuckets(t.Context())
	require.NoError(t, err)
	assert.Nil(t, buckets)

	buckets, err = (&Daemon{rdsService: &handlers_rds.Service{}}).rdsWatchBuckets(t.Context())
	require.NoError(t, err)
	assert.Nil(t, buckets)
}

func TestRdsWatchBuckets_DelegatesToAccountWatchBuckets(t *testing.T) {
	_, _, js := testutil.StartTestJetStream(t)
	_, err := handlers_rds.GetOrCreateAccountBucket(t.Context(), js, "111111111111")
	require.NoError(t, err)

	d := &Daemon{rdsService: &handlers_rds.Service{}, jsManager: &JetStreamManager{js: js}}
	buckets, err := d.rdsWatchBuckets(t.Context())
	require.NoError(t, err)
	require.Len(t, buckets, 1)
	assert.Equal(t, handlers_rds.AccountBucketName("111111111111"), buckets[0].Name())
}
