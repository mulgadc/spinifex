package daemon

// These tests build *Daemon literals directly to reach the lazy watch-bucket
// listers, which is only possible from inside the package.
//test:in-package

import (
	"context"
	"testing"

	"github.com/mulgadc/spinifex/internal/testkit"
	ekscluster "github.com/mulgadc/spinifex/spinifex/domains/eks/cluster"
	elbv2domain "github.com/mulgadc/spinifex/spinifex/domains/elbv2"
	iamdomain "github.com/mulgadc/spinifex/spinifex/domains/iam"
	rdsinstance "github.com/mulgadc/spinifex/spinifex/domains/rds/instance"
	"github.com/mulgadc/spinifex/spinifex/foundation/state/kvstore"
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
		InstanceRecordPrefix + "*",
		elbv2domain.KeyPrefixLB + "*",
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
	d := &Daemon{jsManager: &JetStreamManager{js: js}}

	buckets, err := d.instanceStateWatchBuckets(t.Context())
	require.NoError(t, err)
	require.Len(t, buckets, 1)
	assert.Equal(t, InstanceStateBucket, buckets[0].Name())
}

// TestElbv2WatchBuckets_NoStoreIsNilSafe covers a service that exists but has
// not opened a store, distinct from the elbv2Service field itself being nil.
func TestElbv2WatchBuckets_NoStoreIsNilSafe(t *testing.T) {
	d := &Daemon{elbv2Service: &elbv2domain.ELBv2ServiceImpl{}}
	buckets, err := d.elbv2WatchBuckets(t.Context())
	require.NoError(t, err)
	assert.Nil(t, buckets)
}

func TestElbv2WatchBuckets_WithStoreReturnsWatchBucket(t *testing.T) {
	_, nc, _ := testutil.StartTestJetStream(t)
	masterKey, err := iamdomain.GenerateMasterKey()
	require.NoError(t, err)
	svc, err := elbv2domain.NewELBv2ServiceImplWithNATS(nil, nc, masterKey)
	require.NoError(t, err)
	t.Cleanup(svc.Close)

	d := &Daemon{elbv2Service: svc}
	buckets, err := d.elbv2WatchBuckets(t.Context())
	require.NoError(t, err)
	require.Len(t, buckets, 1)
	assert.Equal(t, elbv2domain.KVBucketELBv2, buckets[0].Name())
}

// TestEksWatchBuckets_NilGuards exercises the two halves of the guard clause
// independently: a natsConn with no service, and a service with no natsConn.
func TestEksWatchBuckets_NilGuards(t *testing.T) {
	_, nc, _ := testutil.StartTestJetStream(t)

	buckets, err := (&Daemon{natsConn: nc}).eksWatchBuckets(t.Context())
	require.NoError(t, err)
	assert.Nil(t, buckets)

	buckets, err = (&Daemon{eksService: &ekscluster.EKSServiceImpl{}}).eksWatchBuckets(t.Context())
	require.NoError(t, err)
	assert.Nil(t, buckets)
}

func TestEksWatchBuckets_DelegatesToAccountWatchBuckets(t *testing.T) {
	_, nc, _ := testutil.StartTestJetStream(t)
	js := testutil.NewJetStream(t, nc)
	_, err := ekscluster.GetOrCreateAccountBucket(t.Context(), js, "111111111111")
	require.NoError(t, err)

	d := &Daemon{eksService: &ekscluster.EKSServiceImpl{}, natsConn: nc}
	buckets, err := d.eksWatchBuckets(t.Context())
	require.NoError(t, err)
	require.Len(t, buckets, 1)
	assert.Equal(t, ekscluster.AccountBucketName("111111111111"), buckets[0].Name())
}

// TestRdsWatchBuckets_NilGuards exercises the two halves of the guard clause
// independently: a jsManager with no service, and a service with no jsManager.
func TestRdsWatchBuckets_NilGuards(t *testing.T) {
	_, _, js := testutil.StartTestJetStream(t)

	buckets, err := (&Daemon{jsManager: &JetStreamManager{js: js}}).rdsWatchBuckets(t.Context())
	require.NoError(t, err)
	assert.Nil(t, buckets)

	buckets, err = (&Daemon{rdsService: &rdsinstance.Service{}}).rdsWatchBuckets(t.Context())
	require.NoError(t, err)
	assert.Nil(t, buckets)
}

func TestRdsWatchBuckets_DelegatesToAccountWatchBuckets(t *testing.T) {
	_, _, js := testutil.StartTestJetStream(t)
	_, err := rdsinstance.GetOrCreateAccountBucket(t.Context(), js, "111111111111")
	require.NoError(t, err)

	d := &Daemon{rdsService: &rdsinstance.Service{}, jsManager: &JetStreamManager{js: js}}
	buckets, err := d.rdsWatchBuckets(t.Context())
	require.NoError(t, err)
	require.Len(t, buckets, 1)
	assert.Equal(t, rdsinstance.AccountBucketName("111111111111"), buckets[0].Name())
}
