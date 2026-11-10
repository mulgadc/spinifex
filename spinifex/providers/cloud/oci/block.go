package oci

import (
	"context"
	"fmt"
	"maps"
	"time"

	"github.com/oracle/oci-go-sdk/v65/common"
	"github.com/oracle/oci-go-sdk/v65/common/auth"
	"github.com/oracle/oci-go-sdk/v65/core"
)

// BlockClient is the narrow block-storage surface an EBS provider needs: OCI
// block volumes, their iSCSI attachments and their backups. It is separate
// from Client so the address allocator and the storage provider stay apart.
type BlockClient interface {
	CreateVolume(ctx context.Context, in CreateVolumeInput) (BlockVolume, error)
	GetVolume(ctx context.Context, volumeID string) (BlockVolume, error)
	// ListVolumes lists the compartment, then keeps only volumes carrying
	// every tag in the filter: OCI's list call has no tag filter of its own.
	// Each volume is listed once, though OCI may list one twice.
	ListVolumes(ctx context.Context, filter VolumeFilter) ([]BlockVolume, error)
	UpdateVolume(ctx context.Context, volumeID string, in UpdateVolumeInput) (BlockVolume, error)
	DeleteVolume(ctx context.Context, volumeID string) error

	AttachVolume(ctx context.Context, in AttachVolumeInput) (VolumeAttachment, error)
	GetVolumeAttachment(ctx context.Context, attachmentID string) (VolumeAttachment, error)
	ListVolumeAttachments(ctx context.Context, filter AttachmentFilter) ([]VolumeAttachment, error)
	DetachVolume(ctx context.Context, attachmentID string) error

	CreateVolumeBackup(ctx context.Context, in CreateVolumeBackupInput) (VolumeBackup, error)
	GetVolumeBackup(ctx context.Context, backupID string) (VolumeBackup, error)
	// ListVolumeBackups filters and lists each backup once, as ListVolumes does.
	ListVolumeBackups(ctx context.Context, filter BackupFilter) ([]VolumeBackup, error)
	DeleteVolumeBackup(ctx context.Context, backupID string) error
}

// VolumeState is an OCI block volume's lifecycle state.
type VolumeState string

const (
	VolumeProvisioning VolumeState = "PROVISIONING"
	VolumeRestoring    VolumeState = "RESTORING"
	VolumeAvailable    VolumeState = "AVAILABLE"
	VolumeTerminating  VolumeState = "TERMINATING"
	VolumeTerminated   VolumeState = "TERMINATED"
	VolumeFaulty       VolumeState = "FAULTY"
)

// Gone reports whether the volume is deleted or being deleted. OCI keeps a
// terminated volume readable and listed for a while after the delete.
func (s VolumeState) Gone() bool { return s == VolumeTerminating || s == VolumeTerminated }

// BackupState is an OCI volume backup's lifecycle state.
type BackupState string

const (
	BackupRequestReceived BackupState = "REQUEST_RECEIVED"
	BackupCreating        BackupState = "CREATING"
	BackupAvailable       BackupState = "AVAILABLE"
	BackupTerminating     BackupState = "TERMINATING"
	BackupTerminated      BackupState = "TERMINATED"
	BackupFaulty          BackupState = "FAULTY"
)

// Gone reports whether the backup is deleted or being deleted.
func (s BackupState) Gone() bool { return s == BackupTerminating || s == BackupTerminated }

// InProgress reports whether OCI is still taking the backup. OCI refuses to
// delete the backup, or its source volume, while this is true.
func (s BackupState) InProgress() bool { return s == BackupRequestReceived || s == BackupCreating }

// AttachmentState is an OCI volume attachment's lifecycle state.
type AttachmentState string

const (
	AttachmentAttaching AttachmentState = "ATTACHING"
	AttachmentAttached  AttachmentState = "ATTACHED"
	AttachmentDetaching AttachmentState = "DETACHING"
	AttachmentDetached  AttachmentState = "DETACHED"
)

// MinVolumeSizeGB is the smallest block volume OCI creates.
const MinVolumeSizeGB = 50

// BlockVolume is an OCI block volume. SourceBackupID is set on a volume
// restored from a backup.
type BlockVolume struct {
	ID                 string
	CompartmentID      string
	AvailabilityDomain string
	DisplayName        string
	SizeGB             int64
	VPUsPerGB          int64
	State              VolumeState
	SourceBackupID     string
	FreeformTags       map[string]string
	TimeCreated        time.Time
}

// CreateVolumeInput creates a volume, empty or restored from SourceBackupID.
// A zero VPUsPerGB takes OCI's default tier. RetryToken is sent as
// opc-retry-token, so OCI answers a repeat with the first call's volume.
type CreateVolumeInput struct {
	CompartmentID      string
	AvailabilityDomain string
	DisplayName        string
	SizeGB             int64
	VPUsPerGB          int64
	SourceBackupID     string
	FreeformTags       map[string]string
	RetryToken         string
}

// VolumeFilter narrows ListVolumes. CompartmentID is required; DisplayName
// and AvailabilityDomain are matched by OCI and Tags by the client.
type VolumeFilter struct {
	CompartmentID      string
	AvailabilityDomain string
	DisplayName        string
	Tags               map[string]string
}

// UpdateVolumeInput changes a volume. A zero SizeGB leaves the size alone and
// a nil FreeformTags leaves the tags alone; a non-nil map replaces them all.
type UpdateVolumeInput struct {
	SizeGB       int64
	FreeformTags map[string]string
}

// VolumeAttachment is a volume attached to an instance. IQN, IPv4 and Port
// are the iSCSI target and are empty on an attachment of another type.
type VolumeAttachment struct {
	ID                 string
	InstanceID         string
	VolumeID           string
	AvailabilityDomain string
	State              AttachmentState
	ReadOnly           bool
	IQN                string
	IPv4               string
	Port               int
}

// AttachVolumeInput attaches VolumeID to InstanceID over iSCSI, never
// shareable, so OCI refuses a second attachment of the volume anywhere.
type AttachVolumeInput struct {
	InstanceID  string
	VolumeID    string
	DisplayName string
	ReadOnly    bool
}

// AttachmentFilter narrows ListVolumeAttachments. CompartmentID is required.
type AttachmentFilter struct {
	CompartmentID string
	VolumeID      string
	InstanceID    string
}

// VolumeBackup is an OCI volume backup. SizeGB is the source volume's size.
type VolumeBackup struct {
	ID            string
	CompartmentID string
	VolumeID      string
	DisplayName   string
	SizeGB        int64
	UniqueSizeGB  int64
	State         BackupState
	FreeformTags  map[string]string
	TimeCreated   time.Time
}

// CreateVolumeBackupInput takes an incremental backup of VolumeID, in the
// volume's compartment. RetryToken is sent as opc-retry-token.
type CreateVolumeBackupInput struct {
	VolumeID     string
	DisplayName  string
	FreeformTags map[string]string
	RetryToken   string
}

// BackupFilter narrows ListVolumeBackups the way VolumeFilter does.
type BackupFilter struct {
	CompartmentID string
	DisplayName   string
	Tags          map[string]string
}

// blockAPIClient is the BlockClient over the OCI Go SDK.
type blockAPIClient struct {
	storage core.BlockstorageClient
	compute core.ComputeClient
}

var _ BlockClient = (*blockAPIClient)(nil)

// NewBlockClientWithProvider builds a BlockClient from an explicit
// configuration provider.
func NewBlockClientWithProvider(p common.ConfigurationProvider) (BlockClient, error) {
	storage, err := core.NewBlockstorageClientWithConfigurationProvider(p)
	if err != nil {
		return nil, fmt.Errorf("oci block storage client: %w", err)
	}
	compute, err := core.NewComputeClientWithConfigurationProvider(p)
	if err != nil {
		return nil, fmt.Errorf("oci compute client: %w", err)
	}
	return &blockAPIClient{storage: storage, compute: compute}, nil
}

// NewInstancePrincipalBlockClient authenticates as the instance itself, as
// NewInstancePrincipalClient does.
func NewInstancePrincipalBlockClient() (BlockClient, error) {
	provider, err := auth.InstancePrincipalConfigurationProvider()
	if err != nil {
		return nil, fmt.Errorf("oci instance principal: %w", err)
	}
	return NewBlockClientWithProvider(provider)
}

// NewConfigFileBlockClient authenticates from an API-key config file, with the
// same defaults as NewConfigFileClient.
func NewConfigFileBlockClient(path, profile string) (BlockClient, error) {
	provider, err := configFileProvider(path, profile)
	if err != nil {
		return nil, err
	}
	return NewBlockClientWithProvider(provider)
}

func (c *blockAPIClient) CreateVolume(ctx context.Context, in CreateVolumeInput) (BlockVolume, error) {
	details := core.CreateVolumeDetails{
		CompartmentId:      &in.CompartmentID,
		AvailabilityDomain: &in.AvailabilityDomain,
		DisplayName:        optional(in.DisplayName),
		SizeInGBs:          &in.SizeGB,
		FreeformTags:       in.FreeformTags,
	}
	if in.VPUsPerGB > 0 {
		details.VpusPerGB = &in.VPUsPerGB
	}
	if in.SourceBackupID != "" {
		details.SourceDetails = core.VolumeSourceFromVolumeBackupDetails{Id: &in.SourceBackupID}
	}
	resp, err := c.storage.CreateVolume(ctx, core.CreateVolumeRequest{
		CreateVolumeDetails: details,
		OpcRetryToken:       optional(in.RetryToken),
	})
	if err != nil {
		return BlockVolume{}, wrap("CreateVolume", err)
	}
	return toBlockVolume(resp.Volume), nil
}

func (c *blockAPIClient) GetVolume(ctx context.Context, volumeID string) (BlockVolume, error) {
	resp, err := c.storage.GetVolume(ctx, core.GetVolumeRequest{VolumeId: &volumeID})
	if err != nil {
		return BlockVolume{}, wrap("GetVolume", err)
	}
	return toBlockVolume(resp.Volume), nil
}

func (c *blockAPIClient) ListVolumes(ctx context.Context, filter VolumeFilter) ([]BlockVolume, error) {
	var out []BlockVolume
	seen := map[string]bool{}
	var page *string
	for {
		resp, err := c.storage.ListVolumes(ctx, core.ListVolumesRequest{
			CompartmentId:      &filter.CompartmentID,
			AvailabilityDomain: optional(filter.AvailabilityDomain),
			DisplayName:        optional(filter.DisplayName),
			Page:               page,
		})
		if err != nil {
			return nil, wrap("ListVolumes", err)
		}
		for _, item := range resp.Items {
			if hasTags(item.FreeformTags, filter.Tags) && firstSight(seen, item.Id) {
				out = append(out, toBlockVolume(item))
			}
		}
		if resp.OpcNextPage == nil {
			return out, nil
		}
		page = resp.OpcNextPage
	}
}

func (c *blockAPIClient) UpdateVolume(ctx context.Context, volumeID string, in UpdateVolumeInput) (BlockVolume, error) {
	details := core.UpdateVolumeDetails{FreeformTags: in.FreeformTags}
	if in.SizeGB > 0 {
		details.SizeInGBs = &in.SizeGB
	}
	resp, err := c.storage.UpdateVolume(ctx, core.UpdateVolumeRequest{VolumeId: &volumeID, UpdateVolumeDetails: details})
	if err != nil {
		return BlockVolume{}, wrap("UpdateVolume", err)
	}
	return toBlockVolume(resp.Volume), nil
}

func (c *blockAPIClient) DeleteVolume(ctx context.Context, volumeID string) error {
	_, err := c.storage.DeleteVolume(ctx, core.DeleteVolumeRequest{VolumeId: &volumeID})
	return wrap("DeleteVolume", err)
}

func (c *blockAPIClient) AttachVolume(ctx context.Context, in AttachVolumeInput) (VolumeAttachment, error) {
	notShareable := false
	resp, err := c.compute.AttachVolume(ctx, core.AttachVolumeRequest{
		AttachVolumeDetails: core.AttachIScsiVolumeDetails{
			InstanceId:  &in.InstanceID,
			VolumeId:    &in.VolumeID,
			DisplayName: optional(in.DisplayName),
			IsReadOnly:  &in.ReadOnly,
			IsShareable: &notShareable,
		},
	})
	if err != nil {
		return VolumeAttachment{}, wrap("AttachVolume", err)
	}
	return toVolumeAttachment(resp.VolumeAttachment), nil
}

func (c *blockAPIClient) GetVolumeAttachment(ctx context.Context, attachmentID string) (VolumeAttachment, error) {
	resp, err := c.compute.GetVolumeAttachment(ctx, core.GetVolumeAttachmentRequest{VolumeAttachmentId: &attachmentID})
	if err != nil {
		return VolumeAttachment{}, wrap("GetVolumeAttachment", err)
	}
	return toVolumeAttachment(resp.VolumeAttachment), nil
}

func (c *blockAPIClient) ListVolumeAttachments(ctx context.Context, filter AttachmentFilter) ([]VolumeAttachment, error) {
	var out []VolumeAttachment
	var page *string
	for {
		resp, err := c.compute.ListVolumeAttachments(ctx, core.ListVolumeAttachmentsRequest{
			CompartmentId: &filter.CompartmentID,
			VolumeId:      optional(filter.VolumeID),
			InstanceId:    optional(filter.InstanceID),
			Page:          page,
		})
		if err != nil {
			return nil, wrap("ListVolumeAttachments", err)
		}
		for _, item := range resp.Items {
			out = append(out, toVolumeAttachment(item))
		}
		if resp.OpcNextPage == nil {
			return out, nil
		}
		page = resp.OpcNextPage
	}
}

func (c *blockAPIClient) DetachVolume(ctx context.Context, attachmentID string) error {
	_, err := c.compute.DetachVolume(ctx, core.DetachVolumeRequest{VolumeAttachmentId: &attachmentID})
	return wrap("DetachVolume", err)
}

func (c *blockAPIClient) CreateVolumeBackup(ctx context.Context, in CreateVolumeBackupInput) (VolumeBackup, error) {
	resp, err := c.storage.CreateVolumeBackup(ctx, core.CreateVolumeBackupRequest{
		CreateVolumeBackupDetails: core.CreateVolumeBackupDetails{
			VolumeId:     &in.VolumeID,
			DisplayName:  optional(in.DisplayName),
			FreeformTags: in.FreeformTags,
			Type:         core.CreateVolumeBackupDetailsTypeIncremental,
		},
		OpcRetryToken: optional(in.RetryToken),
	})
	if err != nil {
		return VolumeBackup{}, wrap("CreateVolumeBackup", err)
	}
	return toVolumeBackup(resp.VolumeBackup), nil
}

func (c *blockAPIClient) GetVolumeBackup(ctx context.Context, backupID string) (VolumeBackup, error) {
	resp, err := c.storage.GetVolumeBackup(ctx, core.GetVolumeBackupRequest{VolumeBackupId: &backupID})
	if err != nil {
		return VolumeBackup{}, wrap("GetVolumeBackup", err)
	}
	return toVolumeBackup(resp.VolumeBackup), nil
}

func (c *blockAPIClient) ListVolumeBackups(ctx context.Context, filter BackupFilter) ([]VolumeBackup, error) {
	var out []VolumeBackup
	seen := map[string]bool{}
	var page *string
	for {
		resp, err := c.storage.ListVolumeBackups(ctx, core.ListVolumeBackupsRequest{
			CompartmentId: &filter.CompartmentID,
			DisplayName:   optional(filter.DisplayName),
			Page:          page,
		})
		if err != nil {
			return nil, wrap("ListVolumeBackups", err)
		}
		for _, item := range resp.Items {
			if hasTags(item.FreeformTags, filter.Tags) && firstSight(seen, item.Id) {
				out = append(out, toVolumeBackup(item))
			}
		}
		if resp.OpcNextPage == nil {
			return out, nil
		}
		page = resp.OpcNextPage
	}
}

func (c *blockAPIClient) DeleteVolumeBackup(ctx context.Context, backupID string) error {
	_, err := c.storage.DeleteVolumeBackup(ctx, core.DeleteVolumeBackupRequest{VolumeBackupId: &backupID})
	return wrap("DeleteVolumeBackup", err)
}

func toBlockVolume(v core.Volume) BlockVolume {
	out := BlockVolume{
		ID:                 deref(v.Id),
		CompartmentID:      deref(v.CompartmentId),
		AvailabilityDomain: deref(v.AvailabilityDomain),
		DisplayName:        deref(v.DisplayName),
		SizeGB:             derefInt64(v.SizeInGBs),
		VPUsPerGB:          derefInt64(v.VpusPerGB),
		State:              VolumeState(v.LifecycleState),
		FreeformTags:       maps.Clone(v.FreeformTags),
	}
	switch src := v.SourceDetails.(type) {
	case core.VolumeSourceFromVolumeBackupDetails:
		out.SourceBackupID = deref(src.Id)
	case *core.VolumeSourceFromVolumeBackupDetails:
		out.SourceBackupID = deref(src.Id)
	}
	if v.TimeCreated != nil {
		out.TimeCreated = v.TimeCreated.Time
	}
	return out
}

func toVolumeAttachment(a core.VolumeAttachment) VolumeAttachment {
	if a == nil {
		return VolumeAttachment{}
	}
	out := VolumeAttachment{
		ID:                 deref(a.GetId()),
		InstanceID:         deref(a.GetInstanceId()),
		VolumeID:           deref(a.GetVolumeId()),
		AvailabilityDomain: deref(a.GetAvailabilityDomain()),
		State:              AttachmentState(a.GetLifecycleState()),
		ReadOnly:           a.GetIsReadOnly() != nil && *a.GetIsReadOnly(),
	}
	var iscsi *core.IScsiVolumeAttachment
	switch v := a.(type) {
	case core.IScsiVolumeAttachment:
		iscsi = &v
	case *core.IScsiVolumeAttachment:
		iscsi = v
	}
	if iscsi != nil {
		out.IQN = deref(iscsi.Iqn)
		out.IPv4 = deref(iscsi.Ipv4)
		if iscsi.Port != nil {
			out.Port = *iscsi.Port
		}
	}
	return out
}

func toVolumeBackup(b core.VolumeBackup) VolumeBackup {
	out := VolumeBackup{
		ID:            deref(b.Id),
		CompartmentID: deref(b.CompartmentId),
		VolumeID:      deref(b.VolumeId),
		DisplayName:   deref(b.DisplayName),
		SizeGB:        derefInt64(b.SizeInGBs),
		UniqueSizeGB:  derefInt64(b.UniqueSizeInGBs),
		State:         BackupState(b.LifecycleState),
		FreeformTags:  maps.Clone(b.FreeformTags),
	}
	if b.TimeCreated != nil {
		out.TimeCreated = b.TimeCreated.Time
	}
	return out
}

// hasTags reports whether tags holds every key and value in want.
func hasTags(tags, want map[string]string) bool {
	for k, v := range want {
		if got, ok := tags[k]; !ok || got != v {
			return false
		}
	}
	return true
}

// optional is nil for the empty string, which the SDK omits from a request.
// firstSight reports whether id is new to seen, and records it. OCI's list
// calls can return a resource twice while it changes state, as a new backup
// leaving REQUEST_RECEIVED does.
func firstSight(seen map[string]bool, id *string) bool {
	key := deref(id)
	if seen[key] {
		return false
	}
	seen[key] = true
	return true
}

func optional(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func derefInt64(n *int64) int64 {
	if n == nil {
		return 0
	}
	return *n
}
