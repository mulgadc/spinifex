package handlers_eks

import (
	"context"
	"testing"

	handlers_dns "github.com/mulgadc/spinifex/spinifex/handlers/dns"
	"github.com/nats-io/nats.go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCreateCluster_EndpointDNSNameUsesOwnerAccount(t *testing.T) {
	fixture := newEKSServiceFixture(t)
	fixture.svc.baseDomain = "spx3.net"

	sub, err := fixture.svc.deps.NATSConn.Subscribe(handlers_dns.SubjectRecordsetChange, func(msg *nats.Msg) {
		_ = msg.Respond([]byte(`{"applied":1}`))
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = sub.Unsubscribe() })
	require.NoError(t, fixture.svc.deps.NATSConn.Flush())

	_, err = fixture.svc.CreateCluster(context.Background(), createInput("alpha"), testAccountID, "")
	require.NoError(t, err)
	fixture.svc.WaitLaunches()

	meta, err := GetClusterMeta(fixture.kv, "alpha")
	require.NoError(t, err)
	assert.Equal(t, "alpha.111122223333.us-east-1.eks.spx3.net", meta.EndpointDNSName)
	assert.Contains(t, meta.Endpoint, meta.EndpointDNSName)
}

func TestDesiredDNSChanges_MetadataReadFailureIsNotAuthoritative(t *testing.T) {
	fixture := newEKSServiceFixture(t)
	fixture.svc.baseDomain = "spx3.net"

	active := sampleClusterMeta("healthy")
	active.Status = ClusterStatusActive
	active.EndpointDNSName = "healthy.111122223333.us-east-1.eks.spx3.net"
	active.EndpointIP = "203.0.113.10"
	require.NoError(t, PutClusterMeta(fixture.kv, active))

	js, err := fixture.svc.deps.NATSConn.JetStream()
	require.NoError(t, err)
	corruptKV, err := GetOrCreateAccountBucket(js, "444455556666", 1)
	require.NoError(t, err)
	_, err = corruptKV.Put(ClusterMetaKey("unreadable"), []byte("{not json"))
	require.NoError(t, err)

	changes, authoritative := fixture.svc.DesiredDNSChanges()
	assert.False(t, authoritative, "a metadata read failure must suppress EKS pruning")
	assert.Nil(t, changes)
}
