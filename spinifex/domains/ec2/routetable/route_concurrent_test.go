package routetable

import (
	"context"
	"sync"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
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
	svc.rtbKV = &simultaneousRouteReads{KeyValue: svc.rtbKV, ready: make(chan struct{})}
	return id
}

// runBoth runs two mutations at once and returns their errors in order.
func runBoth(first, second func() error) [2]error {
	var errs [2]error
	var wg sync.WaitGroup
	for i, fn := range []func() error{first, second} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs[i] = fn()
		}()
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
}
