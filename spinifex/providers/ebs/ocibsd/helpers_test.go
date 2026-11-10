package ocibsd_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/mulgadc/spinifex/spinifex/providers/cloud/oci"
	"github.com/mulgadc/spinifex/spinifex/providers/ebs"
	"github.com/mulgadc/spinifex/spinifex/providers/ebs/ocibsd"
)

const (
	testCluster     = "cluster-1"
	testCompartment = "ocid1.compartment.oc1..test"
	testAD          = "Uocm:US-ASHBURN-AD-1"
	testAZ          = "ap-southeast-2a"
)

// cluster is two nodes' providers over one OCI tenancy, each with its own
// instance and iSCSI initiator.
type cluster struct {
	fake       *oci.BlockFake
	nodes      map[string]*ocibsd.Provider
	initiators map[string]*ocibsd.FakeInitiator
}

func instanceOf(nodeID string) string { return "ocid1.instance.oc1..of-" + nodeID }

func testConfig(nodeID string, client oci.BlockClient, initiator ocibsd.Initiator) ocibsd.Config {
	return ocibsd.Config{
		ClusterID: testCluster, CompartmentID: testCompartment,
		AvailabilityDomain: testAD, AvailabilityZone: testAZ,
		NodeID: nodeID, InstanceID: instanceOf(nodeID), InstanceAvailabilityDomain: testAD,
		Client: client, Initiator: initiator,
		PollInterval: time.Millisecond, VolumeTimeout: time.Second, AttachTimeout: time.Second, DetachTimeout: time.Second,
	}
}

func newCluster(t *testing.T) *cluster {
	t.Helper()
	return newClusterOver(t, oci.NewBlockFake(), nil)
}

// newClusterOver builds the cluster over fake, reached through client when
// one is given, so a test can interpose on the OCI calls.
func newClusterOver(t *testing.T, fake *oci.BlockFake, client oci.BlockClient) *cluster {
	t.Helper()
	if client == nil {
		client = fake
	}
	c := &cluster{fake: fake, nodes: map[string]*ocibsd.Provider{}, initiators: map[string]*ocibsd.FakeInitiator{}}
	for _, node := range []string{"node-1", "node-2"} {
		initiator := ocibsd.NewFakeInitiator()
		p, err := ocibsd.New(testConfig(node, client, initiator))
		require.NoError(t, err)
		c.nodes[node], c.initiators[node] = p, initiator
	}
	return c
}

// router answers the verbs any node can answer on node-1, and sends each
// per-node verb to the node it names, as the NATS subjects do.
type router struct {
	*ocibsd.Provider
	c *cluster
}

var _ ebsprovider.EBSProvider = router{}

func (c *cluster) router() router { return router{Provider: c.nodes["node-1"], c: c} }

func (r router) node(nodeID string) *ocibsd.Provider {
	if p, ok := r.c.nodes[nodeID]; ok {
		return p
	}
	return r.Provider
}

func (r router) PublishVolume(ctx context.Context, req ebsprovider.PublishVolumeRequest) (*ebsprovider.PublishedVolume, error) {
	return r.node(req.NodeID).PublishVolume(ctx, req)
}

func (r router) UnpublishVolume(ctx context.Context, req ebsprovider.UnpublishVolumeRequest) (*ebsprovider.UnpublishedVolume, error) {
	return r.node(req.NodeID).UnpublishVolume(ctx, req)
}

func (r router) HoldsUnsealedWrites(ctx context.Context, nodeID string, volumeIDs []string) ([]string, error) {
	return r.node(nodeID).HoldsUnsealedWrites(ctx, nodeID, volumeIDs)
}

// settling finishes a backup once a filtered read has seen it pending, so a
// wait for a pending snapshot is exercised and still ends.
type settling struct {
	ebsprovider.EBSProvider
	fake *oci.BlockFake
}

func (s settling) ListSnapshots(ctx context.Context, req ebsprovider.ListSnapshotsRequest) (*ebsprovider.ListSnapshotsResponse, error) {
	resp, err := s.EBSProvider.ListSnapshots(ctx, req)
	if err == nil && req.SnapshotID != "" && len(resp.Snapshots) == 1 && resp.Snapshots[0].State == ebsprovider.SnapshotStatePending {
		if err := s.fake.SettleBackup(resp.Snapshots[0].Handle, oci.BackupAvailable); err != nil {
			return nil, err
		}
	}
	return resp, err
}

var latest = ebsprovider.NewLatestVersioned()

func createVolume(t *testing.T, p ebsprovider.EBSProvider, volumeID string, capacity int64) *ebsprovider.Volume {
	t.Helper()
	v, err := p.CreateVolume(context.Background(), ebsprovider.CreateVolumeRequest{
		Versioned: latest, VolumeID: volumeID, CapacityRange: ebsprovider.CapacityRange{RequiredBytes: capacity},
	})
	require.NoError(t, err)
	return v
}

func ociVolume(t *testing.T, fake *oci.BlockFake, handle string) oci.BlockVolume {
	t.Helper()
	v, err := fake.GetVolume(context.Background(), handle)
	require.NoError(t, err)
	return v
}
