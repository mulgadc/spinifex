package handlers_bedrock

import (
	"errors"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/mulgadc/spinifex/spinifex/tags"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLaunchServingVM_WiresLaunchInput(t *testing.T) {
	h := newLaunchHarness()

	out, err := LaunchServingVM(t.Context(), h.deps(), testLaunchInput())
	require.NoError(t, err)
	require.NotNil(t, out)

	in := h.launcher.lastInput()
	require.NotNil(t, in)
	assert.Equal(t, tags.ManagedByBedrock, in.ManagedBy)
	assert.Equal(t, "ami-vllm-serving", in.ImageID)
	assert.Equal(t, "g5.xlarge", in.InstanceType)
	assert.NotEmpty(t, in.ENIID)
	assert.NotEmpty(t, in.ENIIP)
	assert.Contains(t, in.UserData, "VLLM_MODEL_ID="+testModelID)
	assert.Contains(t, in.UserData, "VLLM_ARGS=--dtype=bfloat16")

	assert.Equal(t, out.ENIID, in.ENIID)
	assert.Equal(t, out.PrivateIP, in.ENIIP)
	assert.Equal(t, "http://"+out.PrivateIP+":8000", out.BaseURL)
	assert.NotEmpty(t, out.WeightsVolumeID)
	assert.NotEmpty(t, out.InstanceID)
}

func TestLaunchServingVM_ClonesWeightsSnapshotIntoAVolume(t *testing.T) {
	h := newLaunchHarness()
	h.weights = fakeWeightsResolver{snapshotID: "snap-abc123", resolvable: true}

	out, err := LaunchServingVM(t.Context(), h.deps(), testLaunchInput())
	require.NoError(t, err)

	require.Len(t, h.volumes.created, 1)
	assert.Equal(t, "snap-abc123", aws.StringValue(h.volumes.created[0].SnapshotId))
	assert.NotContains(t, h.volumes.deleted, out.WeightsVolumeID, "must not have been rolled back on success")
}

func TestLaunchServingVM_NoWeightsStaged(t *testing.T) {
	h := newLaunchHarness()
	h.weights = fakeWeightsResolver{resolvable: false}

	_, err := LaunchServingVM(t.Context(), h.deps(), testLaunchInput())
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrNoWeightsStaged)
	assert.Empty(t, h.launcher.launches(), "no VM may be launched without staged weights")
}

func TestLaunchServingVM_RejectsIncompleteInput(t *testing.T) {
	for name, mutate := range map[string]func(*LaunchInput){
		"no model id":      func(in *LaunchInput) { in.ModelID = "" },
		"no instance type": func(in *LaunchInput) { in.InstanceType = "" },
	} {
		t.Run(name, func(t *testing.T) {
			h := newLaunchHarness()
			in := testLaunchInput()
			mutate(&in)

			_, err := LaunchServingVM(t.Context(), h.deps(), in)
			require.Error(t, err)
			assert.Zero(t, h.vpc.nextID, "must fail before touching any provisioner")
		})
	}
}

func TestLaunchServingVM_RollsBackOnVMLaunchFailure(t *testing.T) {
	h := newLaunchHarness()
	h.launcher.failLaunch = errors.New("no capacity")

	_, err := LaunchServingVM(t.Context(), h.deps(), testLaunchInput())
	require.Error(t, err)

	// The ENI and the cloned weights volume must not outlive the failed launch.
	assert.NotEmpty(t, h.vpc.deleted)
	assert.NotEmpty(t, h.volumes.deleted)
}

func TestLaunchServingVM_RollsBackOnAttachFailure(t *testing.T) {
	h := newLaunchHarness()
	h.attacher = &fakeAttacher{failErr: errors.New("no free hot-plug port")}

	_, err := LaunchServingVM(t.Context(), h.deps(), testLaunchInput())
	require.Error(t, err)

	assert.NotEmpty(t, h.launcher.terminated, "a VM with nothing to serve must not be left running")
	assert.NotEmpty(t, h.vpc.deleted)
	assert.NotEmpty(t, h.volumes.deleted)
}

func TestTerminateServingVM_TerminatesDetachesAndDeletes(t *testing.T) {
	h := newLaunchHarness()
	rec := EndpointRecord{
		InstanceID:      "i-42",
		ENIID:           "eni-42",
		WeightsVolumeID: "vol-42",
	}
	err := TerminateServingVM(t.Context(), h.deps(), rec)
	require.NoError(t, err)

	assert.Equal(t, []string{"i-42"}, h.launcher.terminated)
	assert.Contains(t, h.vpc.detached, "eni-42")
	assert.Contains(t, h.vpc.deleted, "eni-42")
	assert.Contains(t, h.volumes.deleted, "vol-42")
}

func TestTerminateServingVM_EmptyRecordIsANoop(t *testing.T) {
	h := newLaunchHarness()
	err := TerminateServingVM(t.Context(), h.deps(), EndpointRecord{})
	require.NoError(t, err)
	assert.Empty(t, h.launcher.terminated)
	assert.Empty(t, h.vpc.deleted)
	assert.Empty(t, h.volumes.deleted)
}
