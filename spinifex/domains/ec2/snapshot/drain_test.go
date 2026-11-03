package snapshot

import (
	"context"
	"sync/atomic"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/contracts/ec2/v1"
	"github.com/mulgadc/spinifex/internal/testkit"
	"github.com/mulgadc/spinifex/spinifex/bootstrap/config"
	"github.com/mulgadc/spinifex/spinifex/domains/ec2/ebs/metadata"
	"github.com/mulgadc/spinifex/spinifex/providers/ebs"
	"github.com/mulgadc/spinifex/spinifex/providers/objectstore"
	"github.com/nats-io/nats.go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const attachedInstanceID = "i-attached-host"

// countingSnapshotProvider counts the CreateSnapshot calls it forwards.
type countingSnapshotProvider struct {
	ebsprovider.EBSProvider

	creates atomic.Int32
}

func (p *countingSnapshotProvider) CreateSnapshot(ctx context.Context, req ebsprovider.CreateSnapshotRequest) (*ebsprovider.Snapshot, error) {
	p.creates.Add(1)
	return p.EBSProvider.CreateSnapshot(ctx, req)
}

// setupAttachedSnapshot is a viperblockd node answering CreateSnapshot for a
// volume attached to a running instance, with a memory provider behind it.
func setupAttachedSnapshot(t *testing.T, volumeID string) (*SnapshotServiceImpl, *countingSnapshotProvider, *nats.Conn) {
	t.Helper()

	_, nc := testutil.StartTestNATS(t)
	cfg := &config.Config{
		DataDir:    testutil.SocketTempDir(t),
		EBS:        config.EBSConfig{Provider: config.EBSProviderViperblockd},
		Predastore: config.PredastoreConfig{Host: "127.0.0.1:1", Bucket: "test-bucket"},
	}
	svc := NewSnapshotServiceImplWithStore(cfg, objectstore.NewMemoryObjectStore(), nc)
	provider := &countingSnapshotProvider{EBSProvider: ebsprovider.NewMemoryProvider(ebsprovider.Capabilities{})}
	svc.SetEBSProvider(provider)

	_, err := provider.CreateVolume(context.Background(), ebsprovider.CreateVolumeRequest{
		Versioned: ebsprovider.NewVersioned(), VolumeID: volumeID,
		CapacityRange:    ebsprovider.CapacityRange{RequiredBytes: 4 * 1024 * 1024 * 1024},
		AvailabilityZone: "us-east-1a",
	})
	require.NoError(t, err)
	require.NoError(t, svc.metadata.PutVolume(context.Background(), ebsmetadata.Volume{
		VolumeID: volumeID, TenantID: testAccountID, CapacityGiB: 4,
		State: "in-use", AttachedInstance: attachedInstanceID, AvailabilityZone: "us-east-1a",
	}))
	return svc, provider, nc
}

// The provider drains a buffered volume inside its own CreateSnapshot, so the
// domain must not dial the drain socket itself: that would drain twice.
func TestCreateSnapshot_AttachedVolumeOpensNoDrainSocket(t *testing.T) {
	svc, provider, _ := setupAttachedSnapshot(t, "vol-attached-local")
	drains := testutil.StartDrainSocket(t, svc.config.DataDir, "vol-attached-local", "OK\n")

	_, err := svc.CreateSnapshot(context.Background(),
		&ec2.CreateSnapshotInput{VolumeId: aws.String("vol-attached-local")}, testAccountID)
	require.NoError(t, err)

	assert.Zero(t, drains(), "the domain must leave the drain to the provider")
	assert.Equal(t, int32(1), provider.creates.Load())
}

// With no drain socket on this node, an attached volume is snapshotted by the
// provider without asking the instance's host for a drain.
func TestCreateSnapshot_AttachedVolumeSendsItsHostNoDrain(t *testing.T) {
	svc, provider, nc := setupAttachedSnapshot(t, "vol-attached-remote")
	var commands atomic.Int32
	sub, err := nc.Subscribe(ec2v1.InstanceCommandSubject(attachedInstanceID), func(msg *nats.Msg) {
		commands.Add(1)
		_ = msg.Respond([]byte(`{"Code":"InternalError","Message":"no drain socket"}`))
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = sub.Unsubscribe() })

	snap, err := svc.CreateSnapshot(context.Background(),
		&ec2.CreateSnapshotInput{VolumeId: aws.String("vol-attached-remote")}, testAccountID)
	require.NoError(t, err)
	assert.Equal(t, "vol-attached-remote", aws.StringValue(snap.VolumeId))
	assert.Equal(t, int32(1), provider.creates.Load())
	assert.Zero(t, commands.Load(), "the instance's host must not be asked to drain")
}
