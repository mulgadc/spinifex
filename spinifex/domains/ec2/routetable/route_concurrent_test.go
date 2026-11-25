package routetable

import (
	"context"
	"sync"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/require"
)

// simultaneousRouteReads holds the first two route-table reads until both have
// happened, so two mutations start from the same revision. A blind Put then
// deterministically discards the change of whichever call writes first.
type simultaneousRouteReads struct {
	jetstream.KeyValue

	mu    sync.Mutex
	reads int
	ready chan struct{}

	// The first two conditional writes are held the same way, so both CAS
	// attempts read one revision and the losing call must retry.
	writes     []uint64
	writeReady chan struct{}
}

func (kv *simultaneousRouteReads) Update(ctx context.Context, key string, value []byte, last uint64) (uint64, error) {
	kv.mu.Lock()
	kv.writes = append(kv.writes, last)
	n := len(kv.writes)
	if n == 2 {
		close(kv.writeReady)
	}
	kv.mu.Unlock()
	if n <= 2 {
		select {
		case <-kv.writeReady:
		case <-ctx.Done():
			return 0, ctx.Err()
		}
	}
	return kv.KeyValue.Update(ctx, key, value, last)
}

// requireRetried asserts that the first two writes raced on one revision and
// that exactly one retry followed.
func requireRetried(t *testing.T, svc *RouteTableServiceImpl) {
	t.Helper()
	kv := svc.rtbKV.(*simultaneousRouteReads)
	kv.mu.Lock()
	defer kv.mu.Unlock()
	require.Len(t, kv.writes, 3, "two racing writes and one retry")
	require.Equal(t, kv.writes[0], kv.writes[1], "both first writes expect the same revision")
	require.Greater(t, kv.writes[2], kv.writes[0], "the retry expects the winner's revision")
}

func (kv *simultaneousRouteReads) Get(ctx context.Context, key string) (jetstream.KeyValueEntry, error) {
	entry, err := kv.KeyValue.Get(ctx, key)
	kv.mu.Lock()
	kv.reads++
	first := kv.reads <= 2
	if kv.reads == 2 {
		close(kv.ready)
	}
	kv.mu.Unlock()
	if first {
		select {
		case <-kv.ready:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return entry, err
}

// seedRoutes adds routes to a new table and arms the same-revision reads.
func seedRoutes(t *testing.T, svc *RouteTableServiceImpl, routes ...RouteRecord) string {
	t.Helper()
	id := createTestRtb(t, svc)
	record, err := svc.getRouteTable(t.Context(), testAccountID, id)
	require.NoError(t, err)
	record.Routes = append(record.Routes, routes...)
	require.NoError(t, svc.putRouteTable(t.Context(), testAccountID, record))
	svc.rtbKV = &simultaneousRouteReads{KeyValue: svc.rtbKV, ready: make(chan struct{}), writeReady: make(chan struct{})}
	return id
}

// runBoth runs two mutations at once and returns their errors in order.
func runBoth(first, second func() error) [2]error {
	var errs [2]error
	var wg sync.WaitGroup
	for i, fn := range []func() error{first, second} {
		wg.Go(func() { errs[i] = fn() })
	}
	wg.Wait()
	return errs
}

func routesByDest(t *testing.T, svc *RouteTableServiceImpl, id string) map[string]RouteRecord {
	t.Helper()
	record, err := svc.getRouteTable(t.Context(), testAccountID, id)
	require.NoError(t, err)
	out := make(map[string]RouteRecord, len(record.Routes))
	for _, r := range record.Routes {
		out[r.DestinationCidrBlock] = r
	}
	return out
}

func TestDeleteRoute_ConcurrentDeletesBothPersist(t *testing.T) {
	svc := setupTestService(t)
	id := seedRoutes(t, svc,
		RouteRecord{DestinationCidrBlock: "0.0.0.0/0", NatGatewayId: "nat-test1", State: "active"},
		RouteRecord{DestinationCidrBlock: "10.100.0.0/16", GatewayId: "igw-test1", State: "active"})

	del := func(cidr string) func() error {
		return func() error {
			_, err := svc.DeleteRoute(t.Context(), &ec2.DeleteRouteInput{
				RouteTableId: aws.String(id), DestinationCidrBlock: aws.String(cidr)}, testAccountID)
			return err
		}
	}
	errs := runBoth(del("0.0.0.0/0"), del("10.100.0.0/16"))
	require.NoError(t, errs[0])
	require.NoError(t, errs[1])

	routes := routesByDest(t, svc, id)
	require.NotContains(t, routes, "0.0.0.0/0")
	require.NotContains(t, routes, "10.100.0.0/16")
	require.Len(t, routes, 1, "only the local route remains")
	requireRetried(t, svc)
}

func TestCreateRoute_ConcurrentCreatesBothPersist(t *testing.T) {
	svc := setupTestService(t)
	id := seedRoutes(t, svc)

	create := func(cidr string) func() error {
		return func() error {
			_, err := svc.CreateRoute(t.Context(), &ec2.CreateRouteInput{
				RouteTableId: aws.String(id), DestinationCidrBlock: aws.String(cidr),
				GatewayId: aws.String("igw-test1")}, testAccountID)
			return err
		}
	}
	errs := runBoth(create("10.100.0.0/16"), create("10.101.0.0/16"))
	require.NoError(t, errs[0])
	require.NoError(t, errs[1])

	routes := routesByDest(t, svc, id)
	require.Equal(t, "igw-test1", routes["10.100.0.0/16"].GatewayId)
	require.Equal(t, "igw-test1", routes["10.101.0.0/16"].GatewayId)
	requireRetried(t, svc)
}

func TestReplaceRoute_ConcurrentWithDeleteRouteBothPersist(t *testing.T) {
	svc := setupTestService(t)
	id := seedRoutes(t, svc,
		RouteRecord{DestinationCidrBlock: "10.100.0.0/16", NatGatewayId: "nat-test1", State: "active"},
		RouteRecord{DestinationCidrBlock: "10.101.0.0/16", GatewayId: "igw-test1", State: "active"})

	errs := runBoth(
		func() error {
			_, err := svc.ReplaceRoute(t.Context(), &ec2.ReplaceRouteInput{
				RouteTableId: aws.String(id), DestinationCidrBlock: aws.String("10.100.0.0/16"),
				GatewayId: aws.String("igw-test1")}, testAccountID)
			return err
		},
		func() error {
			_, err := svc.DeleteRoute(t.Context(), &ec2.DeleteRouteInput{
				RouteTableId: aws.String(id), DestinationCidrBlock: aws.String("10.101.0.0/16")}, testAccountID)
			return err
		})
	require.NoError(t, errs[0])
	require.NoError(t, errs[1])

	routes := routesByDest(t, svc, id)
	require.NotContains(t, routes, "10.101.0.0/16", "the deleted route stays deleted")
	require.Equal(t, "igw-test1", routes["10.100.0.0/16"].GatewayId, "the replaced target stays replaced")
	require.Empty(t, routes["10.100.0.0/16"].NatGatewayId)
	requireRetried(t, svc)
}

// eventsAtWrite records how many add-igw-route events were already queued
// when the route-table record was written.
type eventsAtWrite struct {
	jetstream.KeyValue

	t      *testing.T
	svc    *RouteTableServiceImpl
	sub    *nats.Subscription
	queued []int
}

func (kv *eventsAtWrite) Update(ctx context.Context, key string, value []byte, last uint64) (uint64, error) {
	kv.queued = append(kv.queued, pendingEvents(kv.t, kv.svc, kv.sub))
	return kv.KeyValue.Update(ctx, key, value, last)
}

// TestCreateRoute_PublishesAddEventBeforeWrite checks that CreateRoute still
// publishes the add event before it writes the route.
func TestCreateRoute_PublishesAddEventBeforeWrite(t *testing.T) {
	t.Parallel()
	svc := setupTestService(t)
	rtbID := createTestRtb(t, svc)
	_, err := svc.AssociateRouteTable(t.Context(), &ec2.AssociateRouteTableInput{
		RouteTableId: aws.String(rtbID),
		SubnetId:     aws.String("subnet-priv1"),
	}, testAccountID)
	require.NoError(t, err)

	sub, err := svc.natsConn.SubscribeSync("vpc.add-igw-route")
	require.NoError(t, err)
	defer func() { _ = sub.Unsubscribe() }()
	kv := &eventsAtWrite{KeyValue: svc.rtbKV, t: t, svc: svc, sub: sub}
	svc.rtbKV = kv

	_, err = svc.CreateRoute(t.Context(), &ec2.CreateRouteInput{
		RouteTableId:         aws.String(rtbID),
		DestinationCidrBlock: aws.String("0.0.0.0/0"),
		GatewayId:            aws.String("igw-test1"),
	}, testAccountID)
	require.NoError(t, err)
	require.Equal(t, []int{1}, kv.queued, "the add event must be queued before the single record write")
}
