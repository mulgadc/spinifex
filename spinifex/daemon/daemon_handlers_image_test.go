package daemon

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	"github.com/mulgadc/spinifex/spinifex/vm"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Tests in this file cover handleEC2CreateImage's stopped-instance LastNode
// gate. The image service always fails downstream here, so "not NotFound"
// proves a node passed the gate, and NotFound proves it declined first.

// seedStoppedInstance writes a stopped VM directly to the daemon's shared KV
// so handleEC2CreateImage's vmMgr lookup misses and falls through to the
// LastNode-gated KV branch.
func seedStoppedInstance(t *testing.T, daemon *Daemon, id, lastNode string) {
	t.Helper()
	stoppedVM := &vm.VM{
		ID:        id,
		Status:    vm.StateStopped,
		AccountID: testAccountID,
		LastNode:  lastNode,
		Instance: &ec2.Instance{
			InstanceId: aws.String(id),
			ImageId:    aws.String("ami-source-stopped"),
			BlockDeviceMappings: []*ec2.InstanceBlockDeviceMapping{
				{
					Ebs: &ec2.EbsInstanceBlockDevice{VolumeId: aws.String("vol-" + id)},
				},
			},
		},
	}
	err := daemon.jsManager.WriteStoppedInstance(id, stoppedVM)
	require.NoError(t, err)
	t.Cleanup(func() { _ = daemon.jsManager.DeleteStoppedInstance(id) })
}

// TestHandleEC2CreateImage_StoppedInstance_NonOwnerDeclines checks that a
// node which is not the stopped instance's LastNode declines exactly like
// an unknown instance ID, instead of proceeding into the image service.
func TestHandleEC2CreateImage_StoppedInstance_NonOwnerDeclines(t *testing.T) {
	natsURL := sharedJSNATSURL
	daemon := createFullTestDaemonWithJetStream(t, natsURL)
	// createTestDaemon fixes the cluster config's own node at "node-1".
	seedStoppedInstance(t, daemon, "i-stopped-nonowner", "node-2")

	sub, err := daemon.natsConn.Subscribe("ec2.CreateImage", asMsgHandler(daemon.handleEC2CreateImage))
	require.NoError(t, err)
	defer sub.Unsubscribe()

	input := &ec2.CreateImageInput{
		InstanceId: aws.String("i-stopped-nonowner"),
		Name:       aws.String("efi-verify-stopped-image"),
	}
	reqData, _ := json.Marshal(input)
	reply, err := natsRequest(daemon.natsConn, "ec2.CreateImage", reqData, 5*time.Second)
	require.NoError(t, err)

	var errResp map[string]any
	require.NoError(t, json.Unmarshal(reply.Data, &errResp))
	assert.Equal(t, awserrors.ErrorInvalidInstanceIDNotFound, errResp["Code"],
		"node-1 is not LastNode (node-2) — it must decline instead of creating a duplicate AMI/snapshot")
}

// TestHandleEC2CreateImage_StoppedInstance_OwnerProceeds checks that the
// node recorded as LastNode is let through the ownership gate into the
// image service, rather than declining with NotFound.
func TestHandleEC2CreateImage_StoppedInstance_OwnerProceeds(t *testing.T) {
	natsURL := sharedJSNATSURL
	daemon := createFullTestDaemonWithJetStream(t, natsURL)
	seedStoppedInstance(t, daemon, "i-stopped-owner", daemon.node)

	sub, err := daemon.natsConn.Subscribe("ec2.CreateImage", asMsgHandler(daemon.handleEC2CreateImage))
	require.NoError(t, err)
	defer sub.Unsubscribe()

	input := &ec2.CreateImageInput{
		InstanceId: aws.String("i-stopped-owner"),
		Name:       aws.String("efi-verify-stopped-image"),
	}
	reqData, _ := json.Marshal(input)
	reply, err := natsRequest(daemon.natsConn, "ec2.CreateImage", reqData, 5*time.Second)
	require.NoError(t, err)

	var errResp map[string]any
	require.NoError(t, json.Unmarshal(reply.Data, &errResp))
	assert.NotEqual(t, awserrors.ErrorInvalidInstanceIDNotFound, errResp["Code"],
		"node-1 is LastNode — it must be allowed to proceed into the image service")
}

// TestHandleEC2CreateImage_RunningInstance_LastNodeIgnored checks the
// running-instance path stays governed solely by vmMgr's local-map filter —
// a stale VM.LastNode left over from a prior stop must not cause a decline.
func TestHandleEC2CreateImage_RunningInstance_LastNodeIgnored(t *testing.T) {
	natsURL := sharedNATSURL
	daemon := createFullTestDaemon(t, natsURL)

	daemon.vmMgr.Insert(&vm.VM{
		ID:        "i-running-stale-lastnode",
		Status:    vm.StateRunning,
		AccountID: testAccountID,
		LastNode:  "node-99", // stale value from a previous stop; must be ignored
		Instance: &ec2.Instance{
			InstanceId: aws.String("i-running-stale-lastnode"),
			ImageId:    aws.String("ami-source"),
			BlockDeviceMappings: []*ec2.InstanceBlockDeviceMapping{
				{
					Ebs: &ec2.EbsInstanceBlockDevice{VolumeId: aws.String("vol-running123")},
				},
			},
		},
	})

	sub, err := daemon.natsConn.Subscribe("ec2.CreateImage", asMsgHandler(daemon.handleEC2CreateImage))
	require.NoError(t, err)
	defer sub.Unsubscribe()

	input := &ec2.CreateImageInput{
		InstanceId: aws.String("i-running-stale-lastnode"),
		Name:       aws.String("my-image"),
	}
	reqData, _ := json.Marshal(input)
	reply, err := natsRequest(daemon.natsConn, "ec2.CreateImage", reqData, 5*time.Second)
	require.NoError(t, err)

	var errResp map[string]any
	require.NoError(t, json.Unmarshal(reply.Data, &errResp))
	assert.NotEqual(t, awserrors.ErrorInvalidInstanceIDNotFound, errResp["Code"],
		"running instance found locally in vmMgr must proceed regardless of a stale VM.LastNode")
}

// TestHandleEC2CreateImage_StoppedInstance_EmptyLastNodeElectsSelf checks a
// pre-LastNode legacy record: the requesting node self-elects via CAS
// instead of proceeding unconditionally or declining forever.
func TestHandleEC2CreateImage_StoppedInstance_EmptyLastNodeElectsSelf(t *testing.T) {
	natsURL := sharedJSNATSURL
	daemon := createFullTestDaemonWithJetStream(t, natsURL)
	seedStoppedInstance(t, daemon, "i-stopped-legacy", "") // no LastNode, as an older build would leave it

	sub, err := daemon.natsConn.Subscribe("ec2.CreateImage", asMsgHandler(daemon.handleEC2CreateImage))
	require.NoError(t, err)
	defer sub.Unsubscribe()

	input := &ec2.CreateImageInput{
		InstanceId: aws.String("i-stopped-legacy"),
		Name:       aws.String("efi-verify-stopped-image"),
	}
	reqData, _ := json.Marshal(input)
	reply, err := natsRequest(daemon.natsConn, "ec2.CreateImage", reqData, 5*time.Second)
	require.NoError(t, err)

	var errResp map[string]any
	require.NoError(t, json.Unmarshal(reply.Data, &errResp))
	assert.NotEqual(t, awserrors.ErrorInvalidInstanceIDNotFound, errResp["Code"],
		"the requesting node must self-elect and proceed rather than decline forever")

	updated, err := daemon.jsManager.LoadStoppedInstance("i-stopped-legacy")
	require.NoError(t, err)
	require.NotNil(t, updated)
	assert.Equal(t, daemon.node, updated.LastNode,
		"self-election must persist LastNode in the KV so later requests see a resolved owner")
}
