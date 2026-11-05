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
			RebootInstance:              true,
			AttachENI:                   true,
			DetachENI:                   true,
			AssociateIamInstanceProfile: true,
			SetSpotLineage:              true,
			SetInstanceTags:             true,
			RemoveInstanceTags:          true,
			SetInstanceMonitoring:       true,

			ModifyInstanceMetadataOptions: true,
			SetDisableApiTermination:      true,
		},
		AttachVolumeData:          &AttachVolumeData{VolumeID: "vol-1", Device: "/dev/sdf"},
		DetachVolumeData:          &DetachVolumeData{VolumeID: "vol-2", Device: "/dev/sdg", Force: true},
		AttachENIData:             &AttachENIData{NetworkInterfaceID: "eni-1", DeviceIndex: 2},
		DetachENIData:             &DetachENIData{AttachmentID: "eni-attach-1", Force: true},
		IamProfileAssociationData: &IamProfileAssociationData{InstanceProfileArn: "arn:aws:iam::123456789012:instance-profile/example"},
		SpotLineageData:           &SpotLineageData{SpotInstanceRequestId: "sir-1"},
		InstanceTagsData:          &InstanceTagsData{Tags: map[string]string{"Name": "example"}, TagKeys: []string{"old"}},
		InstanceMonitoringData:    &InstanceMonitoringData{Enabled: true},

		InstanceMetadataOptionsData: &InstanceMetadataOptionsData{
			HttpTokens:              "required",
			HttpEndpoint:            "enabled",
			HttpProtocolIpv6:        "disabled",
			InstanceMetadataTags:    "enabled",
			HttpPutResponseHopLimit: int64Ptr(3),
		},
		DisableApiTerminationData: &DisableApiTerminationData{Value: false},
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
			"reboot_instance": true,
			"attach_eni": true,
			"detach_eni": true,
			"associate_iam_instance_profile": true,
			"set_spot_lineage": true,
			"set_instance_tags": true,
			"remove_instance_tags": true,
			"set_instance_monitoring": true,
			"modify_instance_metadata_options": true,
			"set_disable_api_termination": true
		},
		"attach_volume_data": {"volume_id": "vol-1", "device": "/dev/sdf"},
		"detach_volume_data": {"volume_id": "vol-2", "device": "/dev/sdg", "force": true},
		"attach_eni_data": {"network_interface_id": "eni-1", "device_index": 2},
		"detach_eni_data": {"attachment_id": "eni-attach-1", "force": true},
		"iam_profile_association_data": {"instance_profile_arn": "arn:aws:iam::123456789012:instance-profile/example"},
		"spot_lineage_data": {"spot_instance_request_id": "sir-1"},
		"instance_tags_data": {"tags": {"Name": "example"}, "tag_keys": ["old"]},
		"instance_monitoring_data": {"enabled": true},
		"instance_metadata_options_data": {
			"http_tokens": "required",
			"http_endpoint": "enabled",
			"http_protocol_ipv6": "disabled",
			"instance_metadata_tags": "enabled",
			"http_put_response_hop_limit": 3
		},
		"disable_api_termination_data": {"value": false}
	}`, string(data))
}

func int64Ptr(v int64) *int64 { return &v }

// An owner that predates the metadata and termination-protection commands
// sends and receives payloads without the new fields.
func TestEC2InstanceCommandNewFieldsAreOptionalOnTheWire(t *testing.T) {
	var n1 EC2InstanceCommand
	require.NoError(t, json.Unmarshal([]byte(`{
		"id": "i-0123456789abcdef0",
		"attributes": {"stop_instance": true},
		"instance_monitoring_data": {"enabled": true}
	}`), &n1))
	require.Equal(t, "i-0123456789abcdef0", n1.ID)
	require.True(t, n1.Attributes.StopInstance)
	require.False(t, n1.Attributes.ModifyInstanceMetadataOptions)
	require.False(t, n1.Attributes.SetDisableApiTermination)
	require.Nil(t, n1.InstanceMetadataOptionsData)
	require.Nil(t, n1.DisableApiTerminationData)

	unset, err := json.Marshal(EC2InstanceCommand{ID: "i-1", Attributes: EC2CommandAttributes{StopInstance: true}})
	require.NoError(t, err)
	require.NotContains(t, string(unset), "modify_instance_metadata_options")
	require.NotContains(t, string(unset), "set_disable_api_termination")
	require.NotContains(t, string(unset), "instance_metadata_options_data")
	require.NotContains(t, string(unset), "disable_api_termination_data")
}

func TestInstanceMetadataOptionsDataOmitsUnsetFields(t *testing.T) {
	data, err := json.Marshal(InstanceMetadataOptionsData{HttpPutResponseHopLimit: int64Ptr(1)})
	require.NoError(t, err)
	require.JSONEq(t, `{"http_put_response_hop_limit": 1}`, string(data))

	data, err = json.Marshal(InstanceMetadataOptionsData{})
	require.NoError(t, err)
	require.JSONEq(t, `{}`, string(data))
}

func TestDisableApiTerminationDataKeepsExplicitFalse(t *testing.T) {
	data, err := json.Marshal(DisableApiTerminationData{Value: false})
	require.NoError(t, err)
	require.JSONEq(t, `{"value": false}`, string(data))
}

func TestHandledHeaderName(t *testing.T) {
	require.Equal(t, "Spinifex-Ec2-Cmd-Handled", HandledHeader)
}

func TestInstanceCommandSubjects(t *testing.T) {
	require.Equal(t, "ec2.cmd.i-0123456789abcdef0", InstanceCommandSubject("i-0123456789abcdef0"))
	require.Equal(t, "ec2.cmd.*", InstanceCommandSubjectWildcard)
}
