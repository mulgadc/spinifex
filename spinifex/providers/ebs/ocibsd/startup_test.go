package ocibsd_test

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mulgadc/spinifex/spinifex/providers/cloud/oci"
	"github.com/mulgadc/spinifex/spinifex/providers/ebs"
	"github.com/mulgadc/spinifex/spinifex/providers/ebs/ocibsd"
)

func seedVolume(t *testing.T, fake *oci.BlockFake, cluster string) string {
	t.Helper()
	v, err := fake.CreateVolume(context.Background(), oci.CreateVolumeInput{
		CompartmentID: testCompartment, AvailabilityDomain: testAD, SizeGB: oci.MinVolumeSizeGB,
		FreeformTags: map[string]string{ocibsd.TagCluster: cluster, ocibsd.TagVolumeID: "vol-seeded", ocibsd.TagCapacityBytes: "1073741824"},
	})
	require.NoError(t, err)
	return v.ID
}

func ociTarget(name string) ocibsd.Target {
	return ocibsd.Target{IQN: ocibsd.OCIIQNPrefix + name, IPv4: "169.254.2.2", Port: 3260}
}

func seedAttachment(fake *oci.BlockFake, id, instance, volume string, state oci.AttachmentState, target ocibsd.Target) {
	fake.SeedAttachment(oci.VolumeAttachment{
		ID: id, InstanceID: instance, VolumeID: volume, State: state,
		IQN: target.IQN, IPv4: target.IPv4, Port: target.Port,
	})
}

func TestStartLogsOutOnlyThisClustersDetachedSessions(t *testing.T) {
	cases := []struct {
		name      string
		arrange   func(t *testing.T, fake *oci.BlockFake, target ocibsd.Target)
		target    ocibsd.Target
		loggedOut bool
	}{
		{
			name:   "a tagged volume no longer attached here is logged out",
			target: ociTarget("detached"),
			arrange: func(t *testing.T, fake *oci.BlockFake, target ocibsd.Target) {
				seedAttachment(fake, "att-old", instanceOf("node-1"), seedVolume(t, fake, testCluster), oci.AttachmentDetached, target)
			},
			loggedOut: true,
		},
		{
			name:   "a tagged volume now attached to another node is logged out here",
			target: ociTarget("moved"),
			arrange: func(t *testing.T, fake *oci.BlockFake, target ocibsd.Target) {
				volume := seedVolume(t, fake, testCluster)
				seedAttachment(fake, "att-old", instanceOf("node-1"), volume, oci.AttachmentDetached, target)
				seedAttachment(fake, "att-new", instanceOf("node-2"), volume, oci.AttachmentAttached, target)
			},
			loggedOut: true,
		},
		{
			name:   "a volume still attached to this node is kept",
			target: ociTarget("attached"),
			arrange: func(t *testing.T, fake *oci.BlockFake, target ocibsd.Target) {
				seedAttachment(fake, "att", instanceOf("node-1"), seedVolume(t, fake, testCluster), oci.AttachmentAttached, target)
			},
		},
		{
			name:   "another cluster's volume is kept",
			target: ociTarget("foreign"),
			arrange: func(t *testing.T, fake *oci.BlockFake, target ocibsd.Target) {
				seedAttachment(fake, "att", instanceOf("node-1"), seedVolume(t, fake, "cluster-2"), oci.AttachmentDetached, target)
			},
		},
		{
			name:   "a volume that cannot be read is kept",
			target: ociTarget("unreadable"),
			arrange: func(t *testing.T, fake *oci.BlockFake, target ocibsd.Target) {
				seedAttachment(fake, "att", instanceOf("node-1"), seedVolume(t, fake, testCluster), oci.AttachmentDetached, target)
				fake.FailOp("GetVolume", errors.New("staged: service unavailable"))
			},
		},
		{
			name:    "an OCI IQN with no attachment on record is kept",
			target:  ociTarget("unknown"),
			arrange: func(*testing.T, *oci.BlockFake, ocibsd.Target) {},
		},
		{
			name:   "a target that is not an OCI volume is kept without asking OCI",
			target: ocibsd.Target{IQN: "iqn.2003-01.org.linux-iscsi.san:lun0", IPv4: "10.0.0.5", Port: 3260},
			arrange: func(_ *testing.T, fake *oci.BlockFake, _ ocibsd.Target) {
				fake.FailOp("ListVolumeAttachments", errors.New("staged: must not be called"))
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fake := oci.NewBlockFake()
			tc.arrange(t, fake, tc.target)
			initiator := ocibsd.NewFakeInitiator()
			initiator.SeedSession(tc.target)
			p, err := ocibsd.New(testConfig("node-1", fake, initiator))
			require.NoError(t, err)

			require.NoError(t, p.Start(context.Background()))
			if tc.loggedOut {
				assert.Equal(t, []string{"logout " + tc.target.IQN}, initiator.Events())
				assert.Empty(t, initiator.Open())
			} else {
				assert.Empty(t, initiator.Events())
				assert.Equal(t, []ocibsd.Target{tc.target}, initiator.Open())
			}
		})
	}
}

func TestPublishWaitsForTheStaleSessionLogout(t *testing.T) {
	fake := oci.NewBlockFake()
	stale := ociTarget("stale")
	seedAttachment(fake, "att-old", instanceOf("node-1"), seedVolume(t, fake, testCluster), oci.AttachmentDetached, stale)
	initiator := ocibsd.NewFakeInitiator()
	initiator.SeedSession(stale)
	p, err := ocibsd.New(testConfig("node-1", fake, initiator))
	require.NoError(t, err)
	ctx := context.Background()
	createVolume(t, p, "vol-after", 1<<30)
	publish := ebsprovider.PublishVolumeRequest{Versioned: latest, VolumeID: "vol-after", NodeID: "node-1"}

	_, err = p.PublishVolume(ctx, publish)
	require.ErrorIs(t, err, ebsprovider.ErrUnavailable, "no publish before the stale sessions are gone")
	assert.Empty(t, initiator.Events(), "and no login")

	require.NoError(t, p.Start(ctx))
	_, err = p.PublishVolume(ctx, publish)
	require.NoError(t, err)
	events := initiator.Events()
	require.Len(t, events, 2)
	assert.Equal(t, "logout "+stale.IQN, events[0])
	assert.Regexp(t, `^login `, events[1])
}

func TestStartFailures(t *testing.T) {
	cases := []struct {
		name    string
		arrange func(fake *oci.BlockFake, initiator *ocibsd.FakeInitiator)
		want    string
	}{
		{
			name: "a credential that cannot read volumes fails the start",
			arrange: func(fake *oci.BlockFake, _ *ocibsd.FakeInitiator) {
				fake.FailOp("ListVolumes", errors.New("staged: 404 NotAuthorizedOrNotFound"))
			},
			want: "manage volume-family",
		},
		{
			name: "sessions that cannot be listed fail the start",
			arrange: func(_ *oci.BlockFake, initiator *ocibsd.FakeInitiator) {
				initiator.FailSessions(errors.New("iscsid is not running"))
			},
			want: "list iSCSI sessions",
		},
		{
			name: "attachments that cannot be listed fail the start",
			arrange: func(fake *oci.BlockFake, initiator *ocibsd.FakeInitiator) {
				initiator.SeedSession(ociTarget("any"))
				fake.FailOp("ListVolumeAttachments", errors.New("staged: 500"))
			},
			want: "list volume attachments",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fake := oci.NewBlockFake()
			initiator := ocibsd.NewFakeInitiator()
			tc.arrange(fake, initiator)
			p, err := ocibsd.New(testConfig("node-1", fake, initiator))
			require.NoError(t, err)

			require.ErrorContains(t, p.Start(context.Background()), tc.want)
			_, err = p.PublishVolume(context.Background(), ebsprovider.PublishVolumeRequest{Versioned: latest, VolumeID: "vol-x", NodeID: "node-1"})
			require.ErrorIs(t, err, ebsprovider.ErrUnavailable, "a node that did not start publishes nothing")
			assert.Empty(t, initiator.Events())
		})
	}
}
