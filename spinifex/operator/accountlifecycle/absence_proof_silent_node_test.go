package accountlifecycle

//test:in-package — the volume reaper and its fake cluster are unexported, and
// these drive its force path against a node that never answers.

// Every test here waits out the fixed describe fan-out deadline a silent node
// costs, two or three times over. This file sorts first so they take parallel
// slots before the package's short tests queue ahead of them.

import (
	"testing"

	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A lone node's NotFound says nothing about the node that stayed silent, so
// the holder is not proven gone and the volume keeps its attachment.
func TestVolumeReaperForceRetainsAVolumeWhenALoneNodeAnswersNotFoundWhileAnotherIsSilent(t *testing.T) {
	t.Parallel()
	assertForceRetainsTheHeldVolume(t, func(cluster *fakeCluster) {
		cluster.fail("ec2.DescribeInstances", awserrors.ErrorInvalidInstanceIDNotFound)
	}, []string{fakeClusterNode, "node-2"})
}

// A terminated copy is not proof either while the node that may still run the
// holder has not answered.
func TestVolumeReaperForceRetainsAVolumeWhenATerminatedCopyIsFoundWhileTheHolderNodeIsSilent(t *testing.T) {
	t.Parallel()
	assertForceRetainsTheHeldVolume(t, func(cluster *fakeCluster) {
		cluster.reply("ec2.DescribeTerminatedInstances", instancesIn("terminated", "i-held"))
	}, []string{fakeClusterNode, "node-2"})
}

// The holder lives on node-2, which never answers, while node-1 answers every
// fan-out twice. A count of replies reads that as complete and the holder as
// gone; only judging by responder identity sees that node-2 is missing.
func TestVolumeReaperForceRetainsAVolumeWhenOneNodeAnswersTwiceAndTheHolderNodeIsSilent(t *testing.T) {
	t.Parallel()
	cluster := newFakeCluster(t)
	volume := newHeldVolume(cluster, "vol-held", "i-on-silent-node")
	cluster.echoAsNode(t, "ec2.DescribeInstances", fakeClusterNode)

	reaper := newVolumeReaper(cluster.nc, fakeClusterNode, "node-2")
	err := reaper.Delete(silentClusterCtx(t), "000000000042", Resource{Kind: "volume", ID: "vol-held"}, true)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "vol-held")
	assert.Contains(t, err.Error(), "i-on-silent-node")
	deleted, holder, detaches := volume.state()
	assert.False(t, deleted, "a holder on a silent node is not proven gone")
	assert.Equal(t, "i-on-silent-node", holder)
	assert.Zero(t, detaches)
	assert.NotContains(t, cluster.called(), forceDetachSubject)
}

func TestForcedTeardownKeepsTheAccountWhenOneNodeAnswersTwiceAndTheHolderNodeIsSilent(t *testing.T) {
	t.Parallel()
	cluster := newFakeCluster(t)
	volume := newHeldVolume(cluster, "vol-held", "i-on-silent-node")
	cluster.echoAsNode(t, "ec2.DescribeInstances", fakeClusterNode)

	accounts := newFakeAccounts()
	engine := quietEngine(accounts, newVolumeReaper(cluster.nc, fakeClusterNode, "node-2"))
	req := request()
	req.Force = true

	result, err := engine.Teardown(silentClusterCtx(t), req)

	require.ErrorIs(t, err, ErrResourcesStuck)
	require.Equal(t, 1, result.StuckCount())
	var stuck Stuck
	for _, stage := range result.Stages {
		if len(stage.Stuck) > 0 {
			stuck = stage.Stuck[0]
		}
	}
	assert.Equal(t, "vol-held", stuck.Resource.ID)
	assert.Contains(t, stuck.Reason, "vol-held")
	assert.Contains(t, stuck.Reason, "i-on-silent-node")
	assert.False(t, result.AccountDeleted)
	assert.False(t, accounts.deleted)
	assert.Equal(t, AccountStatusTerminating, accounts.account.Status)
	deleted, holder, detaches := volume.state()
	assert.False(t, deleted)
	assert.Equal(t, "i-on-silent-node", holder)
	assert.Zero(t, detaches)
}
