//go:build e2e

package multinode

import (
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/tests/e2e/harness"
	"github.com/stretchr/testify/require"
)

const (
	mnDataLabel   = "e2emn"
	mnDataSizeMiB = 4
)

// runVolumeDurability proves a volume's bytes follow it across live cluster
// nodes: write a sentinel on an instance hosted by node A, detach, reattach to
// an instance on a *different* node, and read it back. This exercises the
// cluster-wide viperblock master key + predastore-backed data that let a volume
// move between nodes — the assembled path no unit test reaches. Sequential:
// touches predastore + volume state.
func runVolumeDurability(t *testing.T, fix *Fixture) {
	harness.Phase(t, "Multinode — Cross-Node Volume Data Durability")

	az := needAZ(t, fix)
	trio := needInstanceTrio(t, fix)
	require.GreaterOrEqual(t, len(trio), 2, "need >=2 trio instances")
	_, pemPath := needKeyPair(t, fix)

	// Pick two trio instances on distinct cluster nodes.
	srcID, dstID, srcNode, dstNode := pickCrossNodePair(t, fix, trio)
	if srcID == "" {
		t.Skip("trio not spread across >=2 nodes; cannot test cross-node volume mobility")
	}
	harness.Detail(t, "src_instance", srcID, "src_node", srcNode,
		"dst_instance", dstID, "dst_node", dstNode)

	// 1. Create volume → attach to the source instance → write sentinel.
	harness.Step(t, "create-volume size=1 az=%s", az)
	createOut, err := fix.AWS.EC2.CreateVolume(&ec2.CreateVolumeInput{
		AvailabilityZone: aws.String(az),
		Size:             aws.Int64(1),
	})
	require.NoError(t, err, "create-volume")
	volID := aws.StringValue(createOut.VolumeId)
	require.NotEmpty(t, volID, "CreateVolume returned empty VolumeId")
	harness.Detail(t, "volume", volID, "encrypted", aws.BoolValue(createOut.Encrypted))
	harness.RegisterVolumeTeardown(t, fix.AWS, volID)
	harness.WaitForVolumeState(t, fix.AWS, volID, "available",
		harness.WithTimeout(60*time.Second), harness.WithPoll(2*time.Second))

	srcTgt := guestTarget(t, fix, srcID, pemPath)
	before := harness.GuestDiskSet(t, srcTgt)
	harness.AttachVolumeWait(t, fix.AWS, volID, srcID, "/dev/sdf")
	srcDev := harness.WaitForNewGuestDisk(t, srcTgt, before, 90*time.Second)
	wantSha := harness.GuestFormatWriteSentinel(t, srcTgt, srcDev, mnDataLabel, mnDataSizeMiB)
	harness.Detail(t, "src_dev", srcDev, "sha256", wantSha)

	// 2. Detach from A → attach to B (different node) → read back.
	harness.DetachVolumeWait(t, fix.AWS, volID)

	dstTgt := guestTarget(t, fix, dstID, pemPath)
	before = harness.GuestDiskSet(t, dstTgt)
	harness.AttachVolumeWait(t, fix.AWS, volID, dstID, "/dev/sdf")
	dstDev := harness.WaitForNewGuestDisk(t, dstTgt, before, 90*time.Second)
	gotSha := harness.GuestReadSentinelSha(t, dstTgt, "/dev/"+dstDev, mnDataLabel)
	require.Equalf(t, wantSha, gotSha, "sha256 mismatch after cross-node reattach")
	harness.Detail(t, "dst_dev", dstDev, "crossnode_sha_ok", gotSha)
}

// pickCrossNodePair returns the first pair of trio instances hosted by distinct
// cluster nodes. Returns empty src when all instances share one node.
func pickCrossNodePair(t *testing.T, fix *Fixture, trio []string) (srcID, dstID, srcNode, dstNode string) {
	t.Helper()
	nodes := make([]string, len(trio))
	for i, id := range trio {
		n := harness.InstanceHostingNode(t, fix.Cluster, id)
		require.NotNilf(t, n, "no node hosts %s", id)
		nodes[i] = n.Name
	}
	for i := range trio {
		for j := i + 1; j < len(trio); j++ {
			if nodes[i] != nodes[j] {
				return trio[i], trio[j], nodes[i], nodes[j]
			}
		}
	}
	return "", "", "", ""
}

// guestTarget resolves an SSH target for a guest, waiting until SSH answers.
func guestTarget(t *testing.T, fix *Fixture, instanceID, pemPath string) harness.SSHTarget {
	t.Helper()
	host, port := harness.GuestSSHEndpoint(t, fix.AWS, fix.Cluster, instanceID)
	harness.GuestSSHReady(t, host, port, "ec2-user", pemPath,
		harness.WithTimeout(2*time.Minute), harness.WithPoll(2*time.Second))
	return harness.SSHTarget{User: "ec2-user", Host: host, Port: port, KeyPath: pemPath}
}
