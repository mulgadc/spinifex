package types

// EC2InstanceCommand is the NATS wire format for EC2 instance commands
// (stop, terminate, start, attach-volume, detach-volume, attach-eni,
// detach-eni). It replaces direct use of qmp.Command on the gateway→daemon
// boundary.
type EC2InstanceCommand struct {
	ID                        string                     `json:"id"`
	Attributes                EC2CommandAttributes       `json:"attributes"`
	AttachVolumeData          *AttachVolumeData          `json:"attach_volume_data,omitempty"`
	DetachVolumeData          *DetachVolumeData          `json:"detach_volume_data,omitempty"`
	AttachENIData             *AttachENIData             `json:"attach_eni_data,omitempty"`
	DetachENIData             *DetachENIData             `json:"detach_eni_data,omitempty"`
	IamProfileAssociationData *IamProfileAssociationData `json:"iam_profile_association_data,omitempty"`
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
// command. The gateway has already resolved the profile reference to a canonical
// ARN and enforced iam:PassRole — the daemon only needs the ARN to persist on
// vm.VM. The new AssociationId is generated daemon-side under the vmMgr lock,
// so it does not appear in the request payload.
type IamProfileAssociationData struct {
	InstanceProfileArn string `json:"instance_profile_arn"`
}
