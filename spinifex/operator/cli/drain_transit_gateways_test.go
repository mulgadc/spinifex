package cli

import (
	"bytes"
	"context"
	"errors"
	"testing"

	testutil "github.com/mulgadc/spinifex/internal/testkit"
	"github.com/mulgadc/spinifex/spinifex/domains/ec2/routetable"
	ec2tgw "github.com/mulgadc/spinifex/spinifex/domains/ec2/tgw"
	ec2vpc "github.com/mulgadc/spinifex/spinifex/domains/ec2/vpc"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func counted(n int) func(context.Context) (int, error) {
	return func(context.Context) (int, error) { return n, nil }
}

func TestTransitGatewayDrainFailsBeforeConnect(t *testing.T) {
	t.Parallel()
	_, err := (&transitGatewayDrain{}).check(t.Context())
	require.Error(t, err)
}

func TestTransitGatewayDrainSumsAndNamesEachCount(t *testing.T) {
	t.Parallel()
	var out bytes.Buffer
	d := &transitGatewayDrain{live: counted(2), routes: counted(3), marks: counted(5), out: &out}

	n, err := d.check(t.Context())
	require.NoError(t, err)
	assert.Equal(t, 10, n)
	assert.Contains(t, out.String(), "2 not deleted")
	assert.Contains(t, out.String(), "3 route(s) naming one")
	assert.Contains(t, out.String(), "5 VPC or subnet record(s) with a deletion mark")
	assert.Contains(t, out.String(), "Delete every Transit Gateway route and Transit Gateway")
	assert.Contains(t, out.String(), "Retry DeleteVpc or DeleteSubnet")
}

func TestTransitGatewayDrainIdleFormationAsksNothing(t *testing.T) {
	t.Parallel()
	var out bytes.Buffer
	d := &transitGatewayDrain{live: counted(0), routes: counted(0), marks: counted(0), out: &out}

	n, err := d.check(t.Context())
	require.NoError(t, err)
	assert.Zero(t, n)
	assert.NotContains(t, out.String(), "before rolling back")
}

// A count that cannot be read must fail the drain, never read as zero.
func TestTransitGatewayDrainReturnsEachCountError(t *testing.T) {
	t.Parallel()
	boom := errors.New("boom")
	failed := func(context.Context) (int, error) { return 0, boom }
	for name, d := range map[string]*transitGatewayDrain{
		"count transit gateways":       {live: failed, routes: counted(0), marks: counted(0)},
		"count transit gateway routes": {live: counted(0), routes: failed, marks: counted(0)},
		"count deletion marks":         {live: counted(0), routes: counted(0), marks: failed},
	} {
		n, err := d.check(t.Context())
		require.ErrorIs(t, err, boom, name)
		assert.Contains(t, err.Error(), name)
		assert.Zero(t, n, name)
	}
}

func putRaw(t *testing.T, js jetstream.JetStream, bucket, key, value string) {
	t.Helper()
	kv, err := js.KeyValue(t.Context(), bucket)
	if errors.Is(err, jetstream.ErrBucketNotFound) {
		kv, err = js.CreateKeyValue(t.Context(), jetstream.KeyValueConfig{Bucket: bucket})
	}
	require.NoError(t, err)
	_, err = kv.Put(t.Context(), key, []byte(value))
	require.NoError(t, err)
}

// Against the stored records: one live gateway, one route naming it and one
// marked subnet each count; a deleted gateway, a plain route and an unmarked
// VPC do not.
func TestTransitGatewayDrainCountsStoredRecords(t *testing.T) {
	t.Parallel()
	_, _, js := testutil.StartTestJetStream(t)
	const acct = "000000000042"

	putRaw(t, js, ec2tgw.KVBucketTransitGateways, acct+".tgw-0123456789abcdef0", `{"id":"tgw-0123456789abcdef0","state":"available"}`)
	putRaw(t, js, ec2tgw.KVBucketTransitGateways, acct+".tgw-0123456789abcdef1", `{"id":"tgw-0123456789abcdef1","state":"deleted"}`)
	putRaw(t, js, ec2tgw.KVBucketTransitGateways, "acct."+acct+".tgw-0123456789abcdef0", `{"account_id":"`+acct+`"}`)
	putRaw(t, js, routetable.KVBucketRouteTables, acct+".rtb-1", `{"routes":[{"destination_cidr_block":"10.0.0.0/16","gateway_id":"local"},{"destination_cidr_block":"10.1.0.0/16","transit_gateway_id":"tgw-0123456789abcdef0"}]}`)
	putRaw(t, js, ec2vpc.KVBucketVPCs, acct+".vpc-1", `{"vpc_id":"vpc-1"}`)
	putRaw(t, js, ec2vpc.KVBucketSubnets, acct+".subnet-1", `{"subnet_id":"subnet-1","deleting":true}`)

	var out bytes.Buffer
	d := &transitGatewayDrain{}
	d.connect(js, &out)
	n, err := d.check(t.Context())
	require.NoError(t, err)
	assert.Equal(t, 3, n)
	assert.Contains(t, out.String(), "1 not deleted, 1 route(s) naming one, 1 VPC or subnet record(s)")
}

// A formation that never created the buckets has nothing to drain, and the
// drain must not create them.
func TestTransitGatewayDrainMissingBucketsAreEmpty(t *testing.T) {
	t.Parallel()
	_, _, js := testutil.StartTestJetStream(t)
	d := &transitGatewayDrain{}
	d.connect(js, nil)
	n, err := d.check(t.Context())
	require.NoError(t, err)
	assert.Zero(t, n)
	_, err = js.KeyValue(t.Context(), ec2tgw.KVBucketTransitGateways)
	assert.ErrorIs(t, err, jetstream.ErrBucketNotFound)
}
