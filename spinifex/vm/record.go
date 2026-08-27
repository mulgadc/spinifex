package vm

import (
	"time"

	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/spinifex/gpu"
	"github.com/mulgadc/spinifex/spinifex/resource"
	"github.com/mulgadc/spinifex/spinifex/types"
)

// InstanceRecord is the persisted form of an instance under the per-resource
// key space. Nothing reads or writes it yet; it exists so the field-by-field
// boundary can be settled and tested before the keys move onto it.
type InstanceRecord = resource.Object[InstanceSpec, InstanceStatus]

// InstanceSpec is what an operator asked for. It is written by the API layer
// and read by the node; the node never writes it.
type InstanceSpec struct {
	InstanceType string `json:"instance_type,omitempty"`

	// Config is the machine the node builds QEMU from. It is spec because a
	// launch is driven from it and a relaunch on another node replays it, but
	// the launch path appends to Config.NetDevs and Config.Devices, so it is
	// the one spec field a node currently writes.
	Config Config `json:"config"`

	RunInstancesInput *ec2.RunInstancesInput `json:"run_instances_input,omitempty"`

	DesiredState DesiredState `json:"desired_state,omitempty"`
	HostfwdPorts []int        `json:"hostfwd_ports,omitempty"`

	ManagedBy             string `json:"managed_by,omitempty"`
	BootMode              string `json:"boot_mode,omitempty"`
	DirectBoot            bool   `json:"direct_boot,omitempty"`
	IamInstanceProfileArn string `json:"iam_instance_profile_arn,omitempty"`

	PlacementGroupName    string `json:"placement_group_name,omitempty"`
	CapacityReservationId string `json:"capacity_reservation_id,omitempty"`
	InstanceLifecycle     string `json:"instance_lifecycle,omitempty"`
	SpotInstanceRequestId string `json:"spot_instance_request_id,omitempty"`
}

// InstanceStatus is what the node running the instance observed. It is written
// only by that node; the API layer never writes it.
type InstanceStatus struct {
	Status InstanceState `json:"status"`

	// Instance and Reservation are AWS-shaped projections returned verbatim by
	// DescribeInstances, each holding request fields and observed fields in one
	// object. They are the one place the boundary does not hold: building them
	// from spec and status on read is 143 call sites and its own change.
	Instance    *ec2.Instance    `json:"instance,omitempty"`
	Reservation *ec2.Reservation `json:"reservation,omitempty"`

	Health InstanceHealthState `json:"health"`

	// LastNode is the node that last ran this instance, and the field a claim
	// CASes to take ownership. Ownership is carried here rather than in the key.
	LastNode string `json:"last_node,omitempty"`

	MetadataServerAddress string `json:"metadata_server_address,omitempty"`

	ENIId     string     `json:"eni_id,omitempty"`
	ENIMac    string     `json:"eni_mac,omitempty"`
	ExtraENIs []ExtraENI `json:"extra_enis,omitempty"`

	PublicIP        string `json:"public_ip,omitempty"`
	PublicIPPool    string `json:"public_ip_pool,omitempty"`
	PublicIPAllocID string `json:"public_ip_alloc_id,omitempty"`
	PublicIPAssocID string `json:"public_ip_assoc_id,omitempty"`

	DevMAC  string `json:"dev_mac,omitempty"`
	MgmtMAC string `json:"mgmt_mac,omitempty"`
	MgmtIP  string `json:"mgmt_ip,omitempty"`

	PlacementGroupNode string      `json:"placement_group_node,omitempty"`
	HostfwdPortMap     map[int]int `json:"hostfwd_port_map,omitempty"`

	GPUAttachments                  []gpu.GPUAttachment `json:"gpu_attachments,omitempty"`
	IamInstanceProfileAssociationId string              `json:"iam_instance_profile_association_id,omitempty"`

	Teardown       map[string]string `json:"teardown,omitempty"`
	TerminatedAt   time.Time         `json:"terminated_at,omitzero"`
	ShuttingDownAt time.Time         `json:"shutting_down_at,omitzero"`

	// The request slices are carried without their mutexes: a lock is process
	// state, not something a record describes. The wrappers are rebuilt with
	// fresh locks on the way back.
	EBSRequests       []types.EBSRequest `json:"ebs_requests,omitempty"`
	ENIAvailableSlots []int              `json:"eni_available_slots,omitempty"`
	ENIAttachedByID   map[string]int     `json:"eni_attached_by_id,omitempty"`
}

// Record projects a VM onto the persisted record. QMPClient and the request
// mutexes do not cross: they are process state. Neither do the two legacy
// decode-only fields, which exist to fold records at the keys this record
// replaces and have no meaning at the new ones.
func (v *VM) Record() *InstanceRecord {
	return &InstanceRecord{
		Metadata: resource.Metadata{
			Name:              v.ID,
			AccountID:         v.AccountID,
			DeletionTimestamp: v.DeletionTimestamp,
		},
		Spec: InstanceSpec{
			InstanceType:          v.InstanceType,
			Config:                v.Config,
			RunInstancesInput:     v.RunInstancesInput,
			DesiredState:          v.DesiredState,
			HostfwdPorts:          v.HostfwdPorts,
			ManagedBy:             v.ManagedBy,
			BootMode:              v.BootMode,
			DirectBoot:            v.DirectBoot,
			IamInstanceProfileArn: v.IamInstanceProfileArn,
			PlacementGroupName:    v.PlacementGroupName,
			CapacityReservationId: v.CapacityReservationId,
			InstanceLifecycle:     v.InstanceLifecycle,
			SpotInstanceRequestId: v.SpotInstanceRequestId,
		},
		Status: InstanceStatus{
			Status:                          v.Status,
			Instance:                        v.Instance,
			Reservation:                     v.Reservation,
			Health:                          v.Health,
			LastNode:                        v.LastNode,
			MetadataServerAddress:           v.MetadataServerAddress,
			ENIId:                           v.ENIId,
			ENIMac:                          v.ENIMac,
			ExtraENIs:                       v.ExtraENIs,
			PublicIP:                        v.PublicIP,
			PublicIPPool:                    v.PublicIPPool,
			PublicIPAllocID:                 v.PublicIPAllocID,
			PublicIPAssocID:                 v.PublicIPAssocID,
			DevMAC:                          v.DevMAC,
			MgmtMAC:                         v.MgmtMAC,
			MgmtIP:                          v.MgmtIP,
			PlacementGroupNode:              v.PlacementGroupNode,
			HostfwdPortMap:                  v.HostfwdPortMap,
			GPUAttachments:                  v.GPUAttachments,
			IamInstanceProfileAssociationId: v.IamInstanceProfileAssociationId,
			Teardown:                        v.Teardown,
			TerminatedAt:                    v.TerminatedAt,
			ShuttingDownAt:                  v.ShuttingDownAt,
			EBSRequests:                     v.EBSRequests.Requests,
			ENIAvailableSlots:               v.ENIRequests.AvailableSlots,
			ENIAttachedByID:                 v.ENIRequests.AttachedByENIID,
		},
	}
}

// VMFromRecord rebuilds the in-memory instance from a record. QMPClient is left
// nil for the caller to attach, matching what ResetNodeLocalState leaves behind
// after a cross-node move. A function rather than a method: InstanceRecord is an
// alias for an instantiated generic, which cannot carry one.
func VMFromRecord(r *InstanceRecord) *VM {
	v := &VM{
		ID:                r.Metadata.Name,
		AccountID:         r.Metadata.AccountID,
		DeletionTimestamp: r.Metadata.DeletionTimestamp,

		InstanceType:          r.Spec.InstanceType,
		Config:                r.Spec.Config,
		RunInstancesInput:     r.Spec.RunInstancesInput,
		DesiredState:          r.Spec.DesiredState,
		HostfwdPorts:          r.Spec.HostfwdPorts,
		ManagedBy:             r.Spec.ManagedBy,
		BootMode:              r.Spec.BootMode,
		DirectBoot:            r.Spec.DirectBoot,
		IamInstanceProfileArn: r.Spec.IamInstanceProfileArn,
		PlacementGroupName:    r.Spec.PlacementGroupName,
		CapacityReservationId: r.Spec.CapacityReservationId,
		InstanceLifecycle:     r.Spec.InstanceLifecycle,
		SpotInstanceRequestId: r.Spec.SpotInstanceRequestId,

		Status:                          r.Status.Status,
		Instance:                        r.Status.Instance,
		Reservation:                     r.Status.Reservation,
		Health:                          r.Status.Health,
		LastNode:                        r.Status.LastNode,
		MetadataServerAddress:           r.Status.MetadataServerAddress,
		ENIId:                           r.Status.ENIId,
		ENIMac:                          r.Status.ENIMac,
		ExtraENIs:                       r.Status.ExtraENIs,
		PublicIP:                        r.Status.PublicIP,
		PublicIPPool:                    r.Status.PublicIPPool,
		PublicIPAllocID:                 r.Status.PublicIPAllocID,
		PublicIPAssocID:                 r.Status.PublicIPAssocID,
		DevMAC:                          r.Status.DevMAC,
		MgmtMAC:                         r.Status.MgmtMAC,
		MgmtIP:                          r.Status.MgmtIP,
		PlacementGroupNode:              r.Status.PlacementGroupNode,
		HostfwdPortMap:                  r.Status.HostfwdPortMap,
		GPUAttachments:                  r.Status.GPUAttachments,
		IamInstanceProfileAssociationId: r.Status.IamInstanceProfileAssociationId,
		Teardown:                        r.Status.Teardown,
		TerminatedAt:                    r.Status.TerminatedAt,
		ShuttingDownAt:                  r.Status.ShuttingDownAt,
	}
	v.EBSRequests.Requests = r.Status.EBSRequests
	v.ENIRequests.AvailableSlots = r.Status.ENIAvailableSlots
	v.ENIRequests.AttachedByENIID = r.Status.ENIAttachedByID
	return v
}
