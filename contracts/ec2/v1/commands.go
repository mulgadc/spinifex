// Package ec2v1 defines version 1 of the cross-process EC2 instance-command
// contract. These payloads travel on ec2.cmd.<instance-id>; their JSON fields
// are wire compatibility, not an implementation detail of a caller.
package ec2v1

const (
	// InstanceCommandSubjectPrefix is the NATS subject family whose final token
	// identifies the instance-owning daemon.
	InstanceCommandSubjectPrefix = "ec2.cmd."

	// InstanceCommandSubjectWildcard matches exactly one instance-ID token in
	// this contract family. It is the NATS permission/subscription pattern for
	// the per-instance command route, rather than the broader descendant match.
	InstanceCommandSubjectWildcard = InstanceCommandSubjectPrefix + "*"

	// HandledHeader is set to "1" on every reply an owner sends to a
	// modify-instance-metadata-options or set-disable-api-termination command.
	// An owner that predates those commands cannot set it.
	HandledHeader = "Spinifex-Ec2-Cmd-Handled"
)

// InstanceCommandSubject returns the targeted command subject for instanceID.
// It intentionally performs no validation: callers have always supplied the
// already-validated instance identifier directly to NATS, and this structural
// move must not change that behaviour.
func InstanceCommandSubject(instanceID string) string {
	return InstanceCommandSubjectPrefix + instanceID
}

// EC2InstanceCommand is the NATS wire format for EC2 instance commands
// (stop, terminate, start, reboot, attach/detach-volume, attach/detach-eni,
// IAM profile, spot lineage, tags, monitoring, metadata options and
// termination protection).
type EC2InstanceCommand struct {
	ID                        string                     `json:"id"`
	Attributes                EC2CommandAttributes       `json:"attributes"`
	AttachVolumeData          *AttachVolumeData          `json:"attach_volume_data,omitempty"`
	DetachVolumeData          *DetachVolumeData          `json:"detach_volume_data,omitempty"`
	AttachENIData             *AttachENIData             `json:"attach_eni_data,omitempty"`
	DetachENIData             *DetachENIData             `json:"detach_eni_data,omitempty"`
	IamProfileAssociationData *IamProfileAssociationData `json:"iam_profile_association_data,omitempty"`
	SpotLineageData           *SpotLineageData           `json:"spot_lineage_data,omitempty"`
	InstanceTagsData          *InstanceTagsData          `json:"instance_tags_data,omitempty"`
	InstanceMonitoringData    *InstanceMonitoringData    `json:"instance_monitoring_data,omitempty"`

	InstanceMetadataOptionsData *InstanceMetadataOptionsData `json:"instance_metadata_options_data,omitempty"`
	DisableApiTerminationData   *DisableApiTerminationData   `json:"disable_api_termination_data,omitempty"`
}

// EC2CommandAttributes indicates which action the daemon should perform.
type EC2CommandAttributes struct {
	StopInstance                bool `json:"stop_instance"`
	TerminateInstance           bool `json:"delete_instance"`
	StartInstance               bool `json:"start_instance"`
	AttachVolume                bool `json:"attach_volume"`
	DetachVolume                bool `json:"detach_volume"`
	RebootInstance              bool `json:"reboot_instance"`
	AttachENI                   bool `json:"attach_eni"`
	DetachENI                   bool `json:"detach_eni"`
	AssociateIamInstanceProfile bool `json:"associate_iam_instance_profile,omitempty"`
	SetSpotLineage              bool `json:"set_spot_lineage,omitempty"`
	SetInstanceTags             bool `json:"set_instance_tags,omitempty"`
	RemoveInstanceTags          bool `json:"remove_instance_tags,omitempty"`
	SetInstanceMonitoring       bool `json:"set_instance_monitoring,omitempty"`

	ModifyInstanceMetadataOptions bool `json:"modify_instance_metadata_options,omitempty"`
	SetDisableApiTermination      bool `json:"set_disable_api_termination,omitempty"`
}

// AttachVolumeData carries parameters for an attach-volume command.
type AttachVolumeData struct {
	VolumeID string `json:"volume_id"`
	Device   string `json:"device,omitempty"`
}

// DetachVolumeData carries parameters for a detach-volume command.
type DetachVolumeData struct {
	VolumeID string `json:"volume_id"`
	Device   string `json:"device,omitempty"`
	Force    bool   `json:"force,omitempty"`
}

// AttachENIData carries parameters for an attach-network-interface command.
type AttachENIData struct {
	NetworkInterfaceID string `json:"network_interface_id"`
	DeviceIndex        int64  `json:"device_index"`
}

// DetachENIData carries parameters for a detach-network-interface command.
// AttachmentID is the AWS attachment ID returned by AttachNetworkInterface.
type DetachENIData struct {
	AttachmentID string `json:"attachment_id"`
	Force        bool   `json:"force,omitempty"`
}

// IamProfileAssociationData carries parameters for an associate-iam-instance-profile
// command. The gateway resolves the ARN and enforces iam:PassRole; the daemon
// only needs the ARN to persist on vm.VM.
type IamProfileAssociationData struct {
	InstanceProfileArn string `json:"instance_profile_arn"`
}

// InstanceTagsData carries parameters for a set/remove-instance-tags command.
// For create-tags, Tags is the upsert set. For delete-tags, TagKeys removes
// keys unconditionally, Tags removes only on value match, both empty clears all.
type InstanceTagsData struct {
	Tags    map[string]string `json:"tags,omitempty"`
	TagKeys []string          `json:"tag_keys,omitempty"`
}

// InstanceMonitoringData carries the desired EC2 monitoring tier for a
// set-instance-monitoring command: enabled is 60s detailed, disabled is 300s
// basic. Carried as data rather than a second attribute so "disable" is an
// explicit value, not the absence of one.
type InstanceMonitoringData struct {
	Enabled bool `json:"enabled"`
}

// InstanceMetadataOptionsData carries the requested metadata options for a
// modify-instance-metadata-options command. Unset fields are left unchanged.
type InstanceMetadataOptionsData struct {
	HttpTokens              string `json:"http_tokens,omitempty"`
	HttpEndpoint            string `json:"http_endpoint,omitempty"`
	HttpProtocolIpv6        string `json:"http_protocol_ipv6,omitempty"`
	InstanceMetadataTags    string `json:"instance_metadata_tags,omitempty"`
	HttpPutResponseHopLimit *int64 `json:"http_put_response_hop_limit,omitempty"`
}

// DisableApiTerminationData carries the desired termination-protection value
// for a set-disable-api-termination command. Carried as data so "false" is an
// explicit value, not the absence of one.
type DisableApiTerminationData struct {
	Value bool `json:"value"`
}

// SpotLineageData carries the SIR id stamped onto a spot-launched instance.
// Lifecycle is always "spot" for this command, so only the SIR id travels.
type SpotLineageData struct {
	SpotInstanceRequestId string `json:"spot_instance_request_id"`
}
