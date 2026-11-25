package vpcpeering

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// memTagStore is an in-memory central tag index keyed account -> resource.
type memTagStore struct {
	mu   sync.Mutex
	tags map[string]map[string]map[string]string
}

var _ CentralTagStore = (*memTagStore)(nil)

func newMemTagStore() *memTagStore {
	return &memTagStore{tags: map[string]map[string]map[string]string{}}
}

func (m *memTagStore) PutResourceTags(_ context.Context, account, id string, tags map[string]string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.tags[account] == nil {
		m.tags[account] = map[string]map[string]string{}
	}
	m.tags[account][id] = tags
	return nil
}

func (m *memTagStore) DeleteAllTags(_ context.Context, account, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.tags[account], id)
	return nil
}

func (m *memTagStore) ScanTaggedResources(_ context.Context, prefix string) (map[string][]string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := map[string][]string{}
	for account, ids := range m.tags {
		for id := range ids {
			if strings.HasPrefix(id, prefix) {
				out[account] = append(out[account], id)
			}
		}
	}
	return out, nil
}

func (m *memTagStore) get(account, id string) map[string]string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.tags[account][id]
}

func nameTagSpec(name string) []*ec2.TagSpecification {
	return []*ec2.TagSpecification{{
		ResourceType: aws.String(ec2.ResourceTypeVpcPeeringConnection),
		Tags:         []*ec2.Tag{{Key: aws.String("Name"), Value: aws.String(name)}},
	}}
}

// Create-time tags reach the index DescribeTags reads, for a pending
// connection and for one that fails at create.
func TestCreate_WritesTagSpecificationsToCentralStore(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	central := newMemTagStore()
	f.svc.SetCentralTagStore(central)
	f.putVPC(t, testAccount, vpcID(50), "10.1.128.0/24")

	pending, err := f.svc.CreateVpcPeeringConnection(t.Context(), &ec2.CreateVpcPeeringConnectionInput{
		VpcId: aws.String(vpcA), PeerVpcId: aws.String(vpcB), TagSpecifications: nameTagSpec("a-b"),
	}, testAccount)
	require.NoError(t, err)
	failed, err := f.svc.CreateVpcPeeringConnection(t.Context(), &ec2.CreateVpcPeeringConnectionInput{
		VpcId: aws.String(vpcA), PeerVpcId: aws.String(vpcID(50)), TagSpecifications: nameTagSpec("overlap"),
	}, testAccount)
	require.NoError(t, err)

	assert.Equal(t, map[string]string{"Name": "a-b"}, central.get(testAccount, *pending.VpcPeeringConnection.VpcPeeringConnectionId))
	failedID := *failed.VpcPeeringConnection.VpcPeeringConnectionId
	require.Equal(t, StateFailed, f.termRecord(t, failedID).Status)
	assert.Equal(t, map[string]string{"Name": "overlap"}, central.get(testAccount, failedID))
}

// A connection's central tags stay while its record is live or terminal and
// go once the terminal record has aged out of its bucket.
func TestSweep_DeletesCentralTagsOnceTheTerminalRecordIsGone(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	central := newMemTagStore()
	f.svc.SetCentralTagStore(central)
	out, err := f.svc.CreateVpcPeeringConnection(t.Context(), &ec2.CreateVpcPeeringConnectionInput{
		VpcId: aws.String(vpcA), PeerVpcId: aws.String(vpcB), TagSpecifications: nameTagSpec("a-b"),
	}, testAccount)
	require.NoError(t, err)
	id := *out.VpcPeeringConnection.VpcPeeringConnectionId
	keep := *f.create(t, vpcA, vpcC).VpcPeeringConnectionId
	require.NoError(t, central.PutResourceTags(t.Context(), testAccount, keep, map[string]string{"env": "dev"}))
	require.NoError(t, central.PutResourceTags(t.Context(), testAccount, "vpc-0a000000000000001", map[string]string{"Name": "vpc"}))

	res, err := f.svc.Sweep(t.Context())
	require.NoError(t, err)
	assert.Zero(t, res.TagsRemoved, "a live connection keeps its tags")

	require.NoError(t, f.del(t, id))
	require.NotNil(t, f.termRecord(t, id))
	res, err = f.svc.Sweep(t.Context())
	require.NoError(t, err)
	assert.Zero(t, res.TagsRemoved, "a terminal record still in its bucket keeps its tags")
	assert.NotNil(t, central.get(testAccount, id))

	require.NoError(t, f.svc.term.Purge(t.Context(), recordKey(id)))
	res, err = f.svc.Sweep(t.Context())
	require.NoError(t, err)
	assert.Equal(t, 1, res.TagsRemoved)
	assert.Nil(t, central.get(testAccount, id))
	assert.Equal(t, map[string]string{"env": "dev"}, central.get(testAccount, keep))
	assert.Equal(t, map[string]string{"Name": "vpc"}, central.get(testAccount, "vpc-0a000000000000001"))
}
