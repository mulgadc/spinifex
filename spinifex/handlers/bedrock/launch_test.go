package handlers_bedrock

import (
	"context"
	"errors"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	gateway_bedrock "github.com/mulgadc/spinifex/spinifex/gateway/bedrock"
	"github.com/mulgadc/spinifex/spinifex/tags"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// bundleLaunchInput mirrors the real ochre-demo-bundle: one vLLM generative
// member plus two TEI members sharing the VM, standing in for what
// Service.runLaunch actually builds from gateway_bedrock.LookupCoServeGroup.
func bundleLaunchInput() LaunchInput {
	return LaunchInput{
		GroupID:      "ochre-demo-bundle",
		InstanceType: "g5.xlarge",
		Members: []LaunchMemberInput{
			{ModelID: "nomic-embed-text-v1.5", Family: gateway_bedrock.FamilyTEI},
			{ModelID: testModelID, Family: gateway_bedrock.FamilyMeta, VLLMArgs: []string{"--dtype=bfloat16", "--gpu-memory-utilization=0.45"}},
			{ModelID: "bge-reranker-v2-m3", Family: gateway_bedrock.FamilyTEI},
		},
	}
}

// perModelWeightsResolver resolves only the model IDs in ok, standing in for
// a bundle where one member has no staged weights.
type perModelWeightsResolver struct {
	ok map[string]bool
}

func (r perModelWeightsResolver) Resolve(_ context.Context, modelID string) (string, bool, error) {
	if !r.ok[modelID] {
		return "", false, nil
	}
	return "snap-" + modelID, true, nil
}

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
	assert.Contains(t, in.UserData, "BEDROCK_MODEL_ID="+testModelID)
	assert.Contains(t, in.UserData, "BEDROCK_ARGS=--dtype=bfloat16")

	assert.Equal(t, out.ENIID, in.ENIID)
	assert.Equal(t, out.PrivateIP, in.ENIIP)
	require.Contains(t, out.Members, testModelID)
	assert.Equal(t, "http://"+out.PrivateIP+":8000", out.MemberBaseURLs()[testModelID])
	assert.NotEmpty(t, out.Members[testModelID].WeightsVolumeID)
	assert.Equal(t, testModelID, out.PrimaryModelID)
	assert.NotEmpty(t, out.InstanceID)
}

func TestLaunchServingVM_ClonesWeightsSnapshotIntoAVolume(t *testing.T) {
	h := newLaunchHarness()
	h.weights = fakeWeightsResolver{snapshotID: "snap-abc123", resolvable: true}

	out, err := LaunchServingVM(t.Context(), h.deps(), testLaunchInput())
	require.NoError(t, err)

	require.Len(t, h.volumes.created, 1)
	assert.Equal(t, "snap-abc123", aws.StringValue(h.volumes.created[0].SnapshotId))
	assert.NotContains(t, h.volumes.deleted, out.Members[testModelID].WeightsVolumeID, "must not have been rolled back on success")
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
		"no group id":      func(in *LaunchInput) { in.GroupID = "" },
		"no instance type": func(in *LaunchInput) { in.InstanceType = "" },
		"no members":       func(in *LaunchInput) { in.Members = nil },
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

// TestLaunchServingVM_BundleAssignsDeterministicPortsAndDistinctDevices covers
// the multi-service launch: the vLLM member always lands on the well-known
// port and every TEI member on a distinct port from it, each with its own
// weights volume on its own device, and the userData enumerates all three.
func TestLaunchServingVM_BundleAssignsDeterministicPortsAndDistinctDevices(t *testing.T) {
	h := newLaunchHarness()

	out, err := LaunchServingVM(t.Context(), h.deps(), bundleLaunchInput())
	require.NoError(t, err)

	require.Len(t, out.Members, 3)
	assert.Equal(t, vllmServePort, out.Members[testModelID].Port, "the vLLM member must take the well-known port")
	assert.Equal(t, testModelID, out.PrimaryModelID)

	teiPorts := map[int]bool{}
	for _, modelID := range []string{"nomic-embed-text-v1.5", "bge-reranker-v2-m3"} {
		port := out.Members[modelID].Port
		assert.NotEqual(t, vllmServePort, port, "%s must not collide with the vLLM port", modelID)
		assert.False(t, teiPorts[port], "%s must not collide with another TEI member's port", modelID)
		teiPorts[port] = true
	}

	require.Len(t, h.volumes.created, 3, "every member clones its own weights volume")
	volumeIDs := map[string]bool{}
	for _, m := range out.Members {
		require.NotEmpty(t, m.WeightsVolumeID)
		assert.False(t, volumeIDs[m.WeightsVolumeID], "every member must get a distinct weights volume")
		volumeIDs[m.WeightsVolumeID] = true
	}

	in := h.launcher.lastInput()
	require.NotNil(t, in)
	for _, modelID := range []string{testModelID, "nomic-embed-text-v1.5", "bge-reranker-v2-m3"} {
		assert.Contains(t, in.UserData, "BEDROCK_MODEL_ID="+modelID)
	}
	assert.Contains(t, in.UserData, "BEDROCK_ENGINE=vllm")
	assert.Contains(t, in.UserData, "BEDROCK_ENGINE=tei")
}

// TestLaunchServingVM_BundlePartialWeightsRefusesWholeLaunch guards a bundle
// that cannot serve one of its members: the launch must refuse before
// creating anything rather than boot a VM that can only ever serve some of
// its models.
func TestLaunchServingVM_BundlePartialWeightsRefusesWholeLaunch(t *testing.T) {
	h := newLaunchHarness()
	h.weights = perModelWeightsResolver{ok: map[string]bool{testModelID: true, "bge-reranker-v2-m3": true}}

	_, err := LaunchServingVM(t.Context(), h.deps(), bundleLaunchInput())
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrNoWeightsStaged)
	assert.Empty(t, h.launcher.launches(), "no VM may be launched with any member unstaged")
	assert.Empty(t, h.volumes.created, "no weights volume may be cloned for a launch that will be refused")
}

func TestTerminateServingVM_TerminatesDetachesAndDeletes(t *testing.T) {
	h := newLaunchHarness()
	rec := EndpointRecord{
		InstanceID: "i-42",
		ENIID:      "eni-42",
		Members: map[string]MemberEndpoint{
			testModelID: {Port: 8000, WeightsVolumeID: "vol-42"},
		},
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
