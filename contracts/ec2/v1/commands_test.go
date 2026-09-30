package ec2v1

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestEC2InstanceCommandJSONContract(t *testing.T) {
	command := EC2InstanceCommand{
		ID: "i-0123456789abcdef0",
		Attributes: EC2CommandAttributes{
			StopInstance:                true,
			TerminateInstance:           true,
			StartInstance:               true,
			AttachVolume:                true,
			DetachVolume:                true,
			DrainVolume:                 true,
			RebootInstance:              true,
			AttachENI:                   true,
			DetachENI:                   true,
			AssociateIamInstanceProfile: true,
			SetSpotLineage:              true,
			SetInstanceTags:             true,
			RemoveInstanceTags:          true,
			SetInstanceMonitoring:       true,
		},
		AttachVolumeData:          &AttachVolumeData{VolumeID: "vol-1", Device: "/dev/sdf"},
		DetachVolumeData:          &DetachVolumeData{VolumeID: "vol-2", Device: "/dev/sdg", Force: true},
		DrainVolumeData:           &DrainVolumeData{VolumeID: "vol-3"},
		AttachENIData:             &AttachENIData{NetworkInterfaceID: "eni-1", DeviceIndex: 2},
		DetachENIData:             &DetachENIData{AttachmentID: "eni-attach-1", Force: true},
		IamProfileAssociationData: &IamProfileAssociationData{InstanceProfileArn: "arn:aws:iam::123456789012:instance-profile/example"},
		SpotLineageData:           &SpotLineageData{SpotInstanceRequestId: "sir-1"},
		InstanceTagsData:          &InstanceTagsData{Tags: map[string]string{"Name": "example"}, TagKeys: []string{"old"}},
		InstanceMonitoringData:    &InstanceMonitoringData{Enabled: true},
	}

	data, err := json.Marshal(command)
	require.NoError(t, err)
	require.JSONEq(t, `{
		"id": "i-0123456789abcdef0",
		"attributes": {
			"stop_instance": true,
			"delete_instance": true,
			"start_instance": true,
			"attach_volume": true,
			"detach_volume": true,
			"drain_volume": true,
			"reboot_instance": true,
			"attach_eni": true,
			"detach_eni": true,
			"associate_iam_instance_profile": true,
			"set_spot_lineage": true,
			"set_instance_tags": true,
			"remove_instance_tags": true,
			"set_instance_monitoring": true
		},
		"attach_volume_data": {"volume_id": "vol-1", "device": "/dev/sdf"},
		"detach_volume_data": {"volume_id": "vol-2", "device": "/dev/sdg", "force": true},
		"drain_volume_data": {"volume_id": "vol-3"},
		"attach_eni_data": {"network_interface_id": "eni-1", "device_index": 2},
		"detach_eni_data": {"attachment_id": "eni-attach-1", "force": true},
		"iam_profile_association_data": {"instance_profile_arn": "arn:aws:iam::123456789012:instance-profile/example"},
		"spot_lineage_data": {"spot_instance_request_id": "sir-1"},
		"instance_tags_data": {"tags": {"Name": "example"}, "tag_keys": ["old"]},
		"instance_monitoring_data": {"enabled": true}
	}`, string(data))
}

func TestDrainVolumeResponseJSONContract(t *testing.T) {
	data, err := json.Marshal(DrainVolumeResponse{
		VolumeID: "vol-1",
		Status:   DrainVolumeStatusDrained,
	})
	require.NoError(t, err)
	require.JSONEq(t, `{"volume_id":"vol-1","status":"drained"}`, string(data))
	require.Equal(t, "not-running", DrainVolumeStatusNotRunning)
}

func TestInstanceCommandSubjects(t *testing.T) {
	require.Equal(t, "ec2.cmd.i-0123456789abcdef0", InstanceCommandSubject("i-0123456789abcdef0"))
	require.Equal(t, "ec2.cmd.*", InstanceCommandSubjectWildcard)
}
