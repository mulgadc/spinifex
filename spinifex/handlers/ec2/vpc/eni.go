package handlers_ec2_vpc

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	"github.com/mulgadc/spinifex/spinifex/filterutil"
	"github.com/mulgadc/spinifex/spinifex/network/topology"
	"github.com/mulgadc/spinifex/spinifex/utils"
	"github.com/nats-io/nats.go"
)

const (
	KVBucketENIs        = "spinifex-vpc-enis"
	KVBucketENIsVersion = 1
)

// ENIRecord represents a stored Elastic Network Interface
type ENIRecord struct {
	NetworkInterfaceId string `json:"network_interface_id"`
	SubnetId           string `json:"subnet_id"`
	VpcId              string `json:"vpc_id"`
	AvailabilityZone   string `json:"availability_zone"`
	PrivateIpAddress   string `json:"private_ip_address"`
	MacAddress         string `json:"mac_address"`
	Description        string `json:"description"`
	Status             string `json:"status"` // available, in-use
	AttachmentId       string `json:"attachment_id,omitempty"`
	InstanceId         string `json:"instance_id,omitempty"`
	// InstanceOwnerId is the account that owns the attached instance, mirroring
	// AWS's Attachment.InstanceOwnerId. It differs from the ENI's own account only
	// for system VMs (LB/EKS) that plug into a customer-account ENI; IMDS resolves
	// the instance and its IAM role under this account. Empty means same-account.
	InstanceOwnerId  string            `json:"instance_owner_id,omitempty"`
	DeviceIndex      int64             `json:"device_index"`
	PublicIpAddress  string            `json:"public_ip_address,omitempty"` // Auto-assigned or EIP
	PublicIpPool     string            `json:"public_ip_pool,omitempty"`    // Pool name the public IP came from
	SecurityGroupIds []string          `json:"security_group_ids,omitempty"`
	Tags             map[string]string `json:"tags"`
	CreatedAt        time.Time         `json:"created_at"`
	// SourceDestCheck mirrors the AWS ENI attribute; nil means true
	// (AWS default) for records that pre-date the field. Persisted for
	// API read-back parity only — the dataplane does not enforce it.
	SourceDestCheck *bool `json:"source_dest_check,omitempty"`

	// AttachmentStatus carries the hot-plug transition state independent of
	// Status. AWS-parity field: "" (not transitioning), "attaching",
	// "attached", "detaching", "detached". Empty on records with a non-empty
	// AttachmentId pre-dating Sprint 3b is interpreted as "attached".
	AttachmentStatus string `json:"attachment_status,omitempty"`
	// HotPlugSlot is the PCIe root-port slot index (1-based) into which the
	// ENI was hot-plugged. Zero means the ENI is not on the hot-plug pool
	// (boot-time ENI or KV-only attach on a stopped instance).
	HotPlugSlot int `json:"hot_plug_slot,omitempty"`
	// LastAttachError captures the most recent attach-pipeline failure
	// reason so DescribeNetworkInterfaces can surface it. Cleared on the
	// next successful attach.
	LastAttachError string `json:"last_attach_error,omitempty"`
	// DetachInFlight signals that a detach pipeline is mid-flight; persists
	// across daemon restart so the reconciler picks up where it left off.
	DetachInFlight bool `json:"detach_in_flight,omitempty"`
	// DetachForce records the force flag from the in-flight detach call so
	// the reconciler can replay with the original semantics.
	DetachForce bool `json:"detach_force,omitempty"`
	// AttachmentStateAt timestamps the most recent AttachmentStatus
	// transition. The reconciler ages transitions against this so it never
	// rolls back a record a live attach/detach handler is mid-pipeline on.
	AttachmentStateAt time.Time `json:"attachment_state_at,omitzero"`
}

// eniIsLiveAttachment reports whether the ENI record is a live attachment to
// an instance — the rule #3 live reference that pins its subnet undeletable
// and blocks a plain ENI delete. Checks every structured attachment field so a
// single-field drift (e.g. Status cleared but AttachmentId retained) still
// counts. A detached/available ENI is not a live ref: it is itself deletable
// and reaped by the GC backstop, so it never pins its subnet.
func eniIsLiveAttachment(r *ENIRecord) bool {
	return r.Status == "in-use" || r.InstanceId != "" || r.AttachmentId != ""
}

// CreateNetworkInterface creates a new ENI in the specified subnet
func (s *VPCServiceImpl) CreateNetworkInterface(input *ec2.CreateNetworkInterfaceInput, accountID string) (*ec2.CreateNetworkInterfaceOutput, error) {
	if input.SubnetId == nil || *input.SubnetId == "" {
		return nil, errors.New(awserrors.ErrorMissingParameter)
	}

	subnetId := *input.SubnetId

	// Verify subnet exists and belongs to this account
	subnetEntry, err := s.subnetKV.Get(utils.AccountKey(accountID, subnetId))
	if err != nil {
		return nil, errors.New(awserrors.ErrorInvalidSubnetIDNotFound)
	}

	var subnet SubnetRecord
	if err := json.Unmarshal(subnetEntry.Value(), &subnet); err != nil {
		return nil, errors.New(awserrors.ErrorServerInternal)
	}

	var sgIdsIn []string
	for _, id := range input.Groups {
		if id != nil {
			sgIdsIn = append(sgIdsIn, *id)
		}
	}
	if len(sgIdsIn) == 0 {
		// AWS attaches the per-VPC default SG when the caller omits Groups.
		defaultSGId, err := s.FindDefaultSGForVPC(accountID, subnet.VpcId)
		if err != nil || defaultSGId == "" {
			return nil, errors.New(awserrors.ErrorServerInternal)
		}
		sgIdsIn = []string{defaultSGId}
	}
	if err := s.validateSGAttachment(accountID, sgIdsIn, subnet.VpcId); err != nil {
		return nil, err
	}

	eniId := utils.GenerateResourceID("eni")

	// Allocate IP from subnet
	var privateIP string
	if input.PrivateIpAddress != nil && *input.PrivateIpAddress != "" {
		// TODO: validate the requested IP is in the subnet range and not already allocated
		privateIP = *input.PrivateIpAddress
	} else {
		ip, err := s.ipam.AllocateIP(subnetId, subnet.CidrBlock, PurposeENIPrimary, eniId)
		if err != nil {
			return nil, errors.New(awserrors.ErrorServerInternal)
		}
		privateIP = ip
	}

	// Generate a deterministic MAC address
	macAddr := generateENIMac(eniId)

	description := ""
	if input.Description != nil {
		description = *input.Description
	}

	record := ENIRecord{
		NetworkInterfaceId: eniId,
		SubnetId:           subnetId,
		VpcId:              subnet.VpcId,
		AvailabilityZone:   subnet.AvailabilityZone,
		PrivateIpAddress:   privateIP,
		MacAddress:         macAddr,
		Description:        description,
		Status:             "available",
		SecurityGroupIds:   sgIdsIn,
		Tags:               utils.ExtractTags(input.TagSpecifications, "network-interface"),
		CreatedAt:          time.Now(),
	}

	data, err := json.Marshal(record)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal ENI record: %w", err)
	}
	if _, err := s.eniKV.Put(utils.AccountKey(accountID, eniId), data); err != nil {
		return nil, errors.New(awserrors.ErrorServerInternal)
	}

	slog.Info("CreateNetworkInterface completed", "eniId", eniId, "subnetId", subnetId, "ip", privateIP, "accountID", accountID)

	// Send vpc.create-port synchronously so vpcd OVSDB errors surface to the
	// caller. Fire-and-forget would let CreateNetworkInterface return success
	// while the LSP joins zero port groups (NATS hiccup or vpcd OVSDB error),
	// leaving the port unrestricted until the 30s reconciler heals it.
	if err := s.requestPortEvent("vpc.create-port", eniId, subnetId, subnet.VpcId, privateIP, macAddr, sgIdsIn); err != nil {
		slog.Error("CreateNetworkInterface: vpcd create-port failed", "eniId", eniId, "err", err)
		return nil, err
	}

	return &ec2.CreateNetworkInterfaceOutput{
		NetworkInterface: s.eniRecordToEC2(&record, accountID),
	}, nil
}

// DeleteNetworkInterface deletes an ENI. An in-use ENI is rejected; instance
// teardown of its own ENI uses ForceDeleteInstanceENI instead.
func (s *VPCServiceImpl) DeleteNetworkInterface(input *ec2.DeleteNetworkInterfaceInput, accountID string) (*ec2.DeleteNetworkInterfaceOutput, error) {
	if input.NetworkInterfaceId == nil || *input.NetworkInterfaceId == "" {
		return nil, errors.New(awserrors.ErrorMissingParameter)
	}
	return s.deleteNetworkInterface(*input.NetworkInterfaceId, accountID, false)
}

// ForceDeleteInstanceENI deletes an instance's own ENI, bypassing the in-use
// guard. The guard protects against deleting an ENI a *different* live instance
// holds; an instance tearing down its own ENI is always permitted (ADR-0003 §2),
// which breaks the un-terminable-ENI deadlock. Absent is success (idempotent).
func (s *VPCServiceImpl) ForceDeleteInstanceENI(accountID, eniId string) error {
	if eniId == "" {
		return errors.New(awserrors.ErrorMissingParameter)
	}
	_, err := s.deleteNetworkInterface(eniId, accountID, true)
	return err
}

func (s *VPCServiceImpl) deleteNetworkInterface(eniId, accountID string, force bool) (*ec2.DeleteNetworkInterfaceOutput, error) {
	key := utils.AccountKey(accountID, eniId)

	// Get the ENI record
	entry, err := s.eniKV.Get(key)
	if err != nil {
		if errors.Is(err, nats.ErrKeyNotFound) {
			// Internal teardown (force) tolerates an already-gone ENI so
			// instance terminate converges (ADR-0003 §2); the public API is
			// AWS-faithful and returns NotFound.
			if force {
				return &ec2.DeleteNetworkInterfaceOutput{}, nil
			}
			return nil, errors.New(awserrors.ErrorInvalidNetworkInterfaceIDNotFound)
		}
		return nil, errors.New(awserrors.ErrorServerInternal)
	}

	var record ENIRecord
	if err := json.Unmarshal(entry.Value(), &record); err != nil {
		return nil, errors.New(awserrors.ErrorServerInternal)
	}

	// Cannot delete an ENI that is a live attachment unless the owning
	// instance forces teardown. An ENI whose instance is gone is detached by
	// terminate (0003 §2) and falls through as deletable.
	if !force && eniIsLiveAttachment(&record) {
		return nil, errors.New(awserrors.ErrorInvalidNetworkInterfaceInUse)
	}

	// Release the private IP back to the IPAM pool
	if err := s.ipam.ReleaseIP(record.SubnetId, record.PrivateIpAddress); err != nil {
		slog.Warn("Failed to release IP during ENI delete", "eni", eniId, "ip", record.PrivateIpAddress, "err", err)
	}

	// Release auto-assigned public IP (if any) and remove NAT rule.
	// Skip if the public IP belongs to an EIP — those are managed independently.
	if record.PublicIpAddress != "" && s.externalIPAM != nil {
		owned, err := s.isEIPOwned(eniId, accountID)
		if err != nil {
			slog.Error("DeleteNetworkInterface: failed to check EIP ownership, skipping public IP release to avoid data loss", "eniId", eniId, "err", err)
		} else if owned {
			slog.Info("DeleteNetworkInterface: public IP owned by EIP, skipping release", "eniId", eniId, "publicIp", record.PublicIpAddress)
		} else {
			portName := topology.Port(eniId)
			s.publishNATEvent("vpc.delete-nat", record.VpcId, record.PublicIpAddress, record.PrivateIpAddress, portName, record.MacAddress)
			if err := s.externalIPAM.ReleaseIP(record.PublicIpPool, record.PublicIpAddress, eniId); err != nil {
				slog.Warn("Failed to release public IP during ENI delete", "eni", eniId, "ip", record.PublicIpAddress, "pool", record.PublicIpPool, "err", err)
			} else {
				slog.Info("Released auto-assigned public IP during ENI delete", "eniId", eniId, "publicIp", record.PublicIpAddress, "pool", record.PublicIpPool)
			}
		}
	}

	if err := s.eniKV.Delete(key); err != nil {
		return nil, errors.New(awserrors.ErrorServerInternal)
	}

	slog.Info("DeleteNetworkInterface completed", "eniId", eniId, "accountID", accountID)

	// Publish vpc.delete-port event for vpcd topology cleanup. SG IDs are
	// included for consistency with create-port; vpcd's delete handler reads
	// current memberships from the libovsdb cache rather than the event.
	s.publishPortEvent("vpc.delete-port", eniId, record.SubnetId, record.VpcId, record.PrivateIpAddress, record.MacAddress, record.SecurityGroupIds)

	return &ec2.DeleteNetworkInterfaceOutput{}, nil
}

// ModifyNetworkInterfaceAttribute modifies ENI attributes (security groups, description).
func (s *VPCServiceImpl) ModifyNetworkInterfaceAttribute(input *ec2.ModifyNetworkInterfaceAttributeInput, accountID string) (*ec2.ModifyNetworkInterfaceAttributeOutput, error) {
	if input.NetworkInterfaceId == nil || *input.NetworkInterfaceId == "" {
		return nil, errors.New(awserrors.ErrorMissingParameter)
	}
	if len(input.Groups) == 0 && input.Description == nil && input.SourceDestCheck == nil {
		return nil, errors.New(awserrors.ErrorInvalidParameterValue)
	}

	eniId := *input.NetworkInterfaceId
	key := utils.AccountKey(accountID, eniId)

	entry, err := s.eniKV.Get(key)
	if err != nil {
		return nil, errors.New(awserrors.ErrorInvalidNetworkInterfaceIDNotFound)
	}

	var record ENIRecord
	if err := json.Unmarshal(entry.Value(), &record); err != nil {
		slog.Error("ModifyNetworkInterfaceAttribute: corrupted ENI record", "eniId", eniId, "accountID", accountID, "err", err)
		return nil, errors.New(awserrors.ErrorServerInternal)
	}

	sgsChanged := false
	if len(input.Groups) > 0 {
		sgIds := make([]string, 0, len(input.Groups))
		for _, id := range input.Groups {
			if id != nil {
				sgIds = append(sgIds, *id)
			}
		}
		if err := s.validateSGAttachment(accountID, sgIds, record.VpcId); err != nil {
			return nil, err
		}
		record.SecurityGroupIds = sgIds
		sgsChanged = true
	}

	if input.Description != nil && input.Description.Value != nil {
		record.Description = *input.Description.Value
	}

	if input.SourceDestCheck != nil && input.SourceDestCheck.Value != nil {
		record.SourceDestCheck = aws.Bool(*input.SourceDestCheck.Value)
	}

	data, err := json.Marshal(record)
	if err != nil {
		slog.Error("ModifyNetworkInterfaceAttribute: failed to marshal ENI record", "eniId", eniId, "accountID", accountID, "err", err)
		return nil, errors.New(awserrors.ErrorServerInternal)
	}
	if _, err := s.eniKV.Update(key, data, entry.Revision()); err != nil {
		slog.Error("ModifyNetworkInterfaceAttribute: KV update failed", "eniId", eniId, "accountID", accountID, "err", err)
		return nil, errors.New(awserrors.ErrorServerInternal)
	}

	slog.Info("ModifyNetworkInterfaceAttribute completed", "eniId", eniId, "accountID", accountID)

	if sgsChanged {
		if err := s.requestUpdatePortSGsEvent(eniId, record.PrivateIpAddress, record.SecurityGroupIds); err != nil {
			slog.Error("ModifyNetworkInterfaceAttribute: vpcd request failed", "eniId", eniId, "err", err)
			return nil, err
		}
	}

	return &ec2.ModifyNetworkInterfaceAttributeOutput{}, nil
}

var describeNetworkInterfacesValidFilters = map[string]bool{
	"network-interface-id":     true,
	"subnet-id":                true,
	"vpc-id":                   true,
	"status":                   true,
	"private-ip-address":       true,
	"availability-zone":        true,
	"group-id":                 true,
	"mac-address":              true,
	"description":              true,
	"attachment.attachment-id": true,
	"attachment.instance-id":   true,
	"attachment.status":        true,
}

// DescribeNetworkInterfaces lists ENIs with optional filters
func (s *VPCServiceImpl) DescribeNetworkInterfaces(input *ec2.DescribeNetworkInterfacesInput, accountID string) (*ec2.DescribeNetworkInterfacesOutput, error) {
	parsedFilters, err := filterutil.ParseFilters(input.Filters, describeNetworkInterfacesValidFilters)
	if err != nil {
		slog.Warn("DescribeNetworkInterfaces: invalid filter", "err", err)
		return nil, errors.New(awserrors.ErrorInvalidParameterValue)
	}

	enis := make([]*ec2.NetworkInterface, 0)

	eniIDs := make(map[string]bool)
	for _, id := range input.NetworkInterfaceIds {
		if id != nil {
			eniIDs[*id] = true
		}
	}

	prefix := accountID + "."
	keys, err := s.eniKV.Keys()
	if err != nil && !errors.Is(err, nats.ErrNoKeysFound) {
		return nil, errors.New(awserrors.ErrorServerInternal)
	}

	for _, key := range keys {
		if key == utils.VersionKey {
			continue
		}
		if !strings.HasPrefix(key, prefix) {
			continue
		}

		entry, err := s.eniKV.Get(key)
		if err != nil {
			slog.Warn("Failed to get ENI record", "key", key, "error", err)
			continue
		}

		var record ENIRecord
		if err := json.Unmarshal(entry.Value(), &record); err != nil {
			slog.Warn("Failed to unmarshal ENI record", "key", key, "error", err)
			continue
		}

		if len(eniIDs) > 0 && !eniIDs[record.NetworkInterfaceId] {
			continue
		}
		if !eniMatchesFilters(&record, parsedFilters) {
			continue
		}

		enis = append(enis, s.eniRecordToEC2(&record, accountID))
	}

	// If specific ENI IDs were requested but not found, return error
	if len(eniIDs) > 0 {
		found := make(map[string]bool)
		for _, eni := range enis {
			if eni.NetworkInterfaceId != nil {
				found[*eni.NetworkInterfaceId] = true
			}
		}
		for id := range eniIDs {
			if !found[id] {
				return nil, errors.New(awserrors.ErrorInvalidNetworkInterfaceIDNotFound)
			}
		}
	}

	slog.Info("DescribeNetworkInterfaces completed", "count", len(enis), "accountID", accountID)

	return &ec2.DescribeNetworkInterfacesOutput{
		NetworkInterfaces: enis,
	}, nil
}

// eniMatchesFilters checks whether an ENI record matches all parsed filters.
func eniMatchesFilters(record *ENIRecord, filters map[string][]string) bool {
	for name, values := range filters {
		if strings.HasPrefix(name, "tag:") {
			continue
		}
		switch name {
		case "network-interface-id":
			if !filterutil.MatchesAny(values, record.NetworkInterfaceId) {
				return false
			}
		case "subnet-id":
			if !filterutil.MatchesAny(values, record.SubnetId) {
				return false
			}
		case "vpc-id":
			if !filterutil.MatchesAny(values, record.VpcId) {
				return false
			}
		case "status":
			if !filterutil.MatchesAny(values, record.Status) {
				return false
			}
		case "private-ip-address":
			if !filterutil.MatchesAny(values, record.PrivateIpAddress) {
				return false
			}
		case "availability-zone":
			if !filterutil.MatchesAny(values, record.AvailabilityZone) {
				return false
			}
		case "group-id":
			found := false
			for _, sgId := range record.SecurityGroupIds {
				if filterutil.MatchesAny(values, sgId) {
					found = true
					break
				}
			}
			if !found {
				return false
			}
		case "mac-address":
			if !filterutil.MatchesAny(values, record.MacAddress) {
				return false
			}
		case "description":
			if !filterutil.MatchesAny(values, record.Description) {
				return false
			}
		case "attachment.attachment-id":
			if !filterutil.MatchesAny(values, record.AttachmentId) {
				return false
			}
		case "attachment.instance-id":
			if !filterutil.MatchesAny(values, record.InstanceId) {
				return false
			}
		case "attachment.status":
			status := "detached"
			if record.InstanceId != "" {
				status = "attached"
			}
			if !filterutil.MatchesAny(values, status) {
				return false
			}
		default:
			return false
		}
	}
	return filterutil.MatchesTags(filters, record.Tags)
}

// AttachENI marks an ENI as attached to an instance (internal use by RunInstances).
// accountID scopes the lookup to the correct KV key.
func (s *VPCServiceImpl) AttachENI(accountID, eniId, instanceId string, deviceIndex int64) (string, error) {
	key := utils.AccountKey(accountID, eniId)
	entry, err := s.eniKV.Get(key)
	if err != nil {
		return "", errors.New(awserrors.ErrorInvalidNetworkInterfaceIDNotFound)
	}

	var record ENIRecord
	if err := json.Unmarshal(entry.Value(), &record); err != nil {
		return "", errors.New(awserrors.ErrorServerInternal)
	}

	if record.Status == "in-use" {
		return "", errors.New(awserrors.ErrorInvalidNetworkInterfaceInUse)
	}

	attachmentId := utils.GenerateResourceID("eni-attach")
	record.Status = "in-use"
	record.AttachmentId = attachmentId
	record.InstanceId = instanceId
	record.DeviceIndex = deviceIndex

	data, err := json.Marshal(record)
	if err != nil {
		return "", fmt.Errorf("failed to marshal ENI record: %w", err)
	}
	if _, err := s.eniKV.Update(key, data, entry.Revision()); err != nil {
		return "", errors.New(awserrors.ErrorServerInternal)
	}

	slog.Info("ENI attached", "eniId", eniId, "instanceId", instanceId, "attachmentId", attachmentId)
	return attachmentId, nil
}

// DetachENI marks an ENI as detached from an instance (internal use by TerminateInstances).
// accountID scopes the lookup to the correct KV key.
func (s *VPCServiceImpl) DetachENI(accountID, eniId string) error {
	key := utils.AccountKey(accountID, eniId)
	entry, err := s.eniKV.Get(key)
	if err != nil {
		return errors.New(awserrors.ErrorInvalidNetworkInterfaceIDNotFound)
	}

	var record ENIRecord
	if err := json.Unmarshal(entry.Value(), &record); err != nil {
		return errors.New(awserrors.ErrorServerInternal)
	}

	record.Status = "available"
	record.AttachmentId = ""
	record.InstanceId = ""
	record.DeviceIndex = 0

	data, err := json.Marshal(record)
	if err != nil {
		return fmt.Errorf("failed to marshal ENI record: %w", err)
	}
	if _, err := s.eniKV.Update(key, data, entry.Revision()); err != nil {
		return errors.New(awserrors.ErrorServerInternal)
	}

	slog.Info("ENI detached", "eniId", eniId)
	return nil
}

// GetENIRecord reads the ENIRecord for eniId scoped to accountID. Used by
// hot-plug handlers that need the record's MAC + AttachmentId before
// running the QMP pipeline.
func (s *VPCServiceImpl) GetENIRecord(accountID, eniId string) (ENIRecord, error) {
	key := utils.AccountKey(accountID, eniId)
	entry, err := s.eniKV.Get(key)
	if err != nil {
		return ENIRecord{}, errors.New(awserrors.ErrorInvalidNetworkInterfaceIDNotFound)
	}
	var record ENIRecord
	if err := json.Unmarshal(entry.Value(), &record); err != nil {
		return ENIRecord{}, errors.New(awserrors.ErrorServerInternal)
	}
	return record, nil
}

// UpdateENI applies fn to the ENIRecord identified by eniId and persists
// the result under the same revision token (compare-and-swap). fn must
// not block on external I/O; the KV update slot is contended.
func (s *VPCServiceImpl) UpdateENI(accountID, eniId string, fn func(*ENIRecord)) error {
	key := utils.AccountKey(accountID, eniId)
	entry, err := s.eniKV.Get(key)
	if err != nil {
		return errors.New(awserrors.ErrorInvalidNetworkInterfaceIDNotFound)
	}
	var record ENIRecord
	if err := json.Unmarshal(entry.Value(), &record); err != nil {
		return errors.New(awserrors.ErrorServerInternal)
	}
	fn(&record)
	data, err := json.Marshal(record)
	if err != nil {
		return fmt.Errorf("marshal ENI record: %w", err)
	}
	if _, err := s.eniKV.Update(key, data, entry.Revision()); err != nil {
		return errors.New(awserrors.ErrorServerInternal)
	}
	return nil
}

// ListInstanceENIs returns every ENIRecord in accountID currently attached to
// instanceID. Used by the hot-plug reconciler to converge a node's instances
// against KV on restart.
func (s *VPCServiceImpl) ListInstanceENIs(accountID, instanceID string) ([]ENIRecord, error) {
	keys, err := s.eniKV.Keys()
	if err != nil {
		if errors.Is(err, nats.ErrNoKeysFound) {
			return nil, nil
		}
		return nil, errors.New(awserrors.ErrorServerInternal)
	}
	prefix := accountID + "."
	var out []ENIRecord
	for _, key := range keys {
		if key == utils.VersionKey || !strings.HasPrefix(key, prefix) {
			continue
		}
		entry, err := s.eniKV.Get(key)
		if err != nil {
			continue
		}
		var record ENIRecord
		if err := json.Unmarshal(entry.Value(), &record); err != nil {
			continue
		}
		if record.InstanceId == instanceID {
			out = append(out, record)
		}
	}
	return out, nil
}

// FindENIByAttachment scans the ENI bucket for the record with the given
// AttachmentId. Used by DetachNetworkInterface which identifies by attachment ID.
func (s *VPCServiceImpl) FindENIByAttachment(accountID, attachmentId string) (ENIRecord, error) {
	keys, err := s.eniKV.Keys()
	if err != nil && !errors.Is(err, nats.ErrNoKeysFound) {
		return ENIRecord{}, errors.New(awserrors.ErrorServerInternal)
	}
	prefix := accountID + "."
	for _, key := range keys {
		if key == utils.VersionKey || !strings.HasPrefix(key, prefix) {
			continue
		}
		entry, err := s.eniKV.Get(key)
		if err != nil {
			continue
		}
		var record ENIRecord
		if err := json.Unmarshal(entry.Value(), &record); err != nil {
			continue
		}
		if record.AttachmentId == attachmentId {
			return record, nil
		}
	}
	return ENIRecord{}, errors.New(awserrors.ErrorInvalidAttachmentIDNotFound)
}

// UpdateENIPublicIP updates the PublicIpAddress and PublicIpPool on an ENI record.
func (s *VPCServiceImpl) UpdateENIPublicIP(accountID, eniId, publicIP, poolName string) error {
	key := utils.AccountKey(accountID, eniId)
	entry, err := s.eniKV.Get(key)
	if err != nil {
		return fmt.Errorf("ENI %s not found: %w", eniId, err)
	}

	var record ENIRecord
	if err := json.Unmarshal(entry.Value(), &record); err != nil {
		return fmt.Errorf("unmarshal ENI record: %w", err)
	}

	record.PublicIpAddress = publicIP
	record.PublicIpPool = poolName

	data, err := json.Marshal(record)
	if err != nil {
		return fmt.Errorf("marshal ENI record: %w", err)
	}
	if _, err := s.eniKV.Update(key, data, entry.Revision()); err != nil {
		return fmt.Errorf("update ENI record: %w", err)
	}

	slog.Info("Updated ENI with public IP", "eniId", eniId, "publicIp", publicIP, "pool", poolName)
	return nil
}

// eniRecordToEC2 converts an ENI record to an EC2 NetworkInterface
func (s *VPCServiceImpl) eniRecordToEC2(record *ENIRecord, accountID string) *ec2.NetworkInterface {
	// ENIs with spinifex:managed-by tag are system-managed (e.g. by ELBv2)
	requesterManaged := record.Tags["spinifex:managed-by"] != ""

	eni := &ec2.NetworkInterface{
		NetworkInterfaceId: aws.String(record.NetworkInterfaceId),
		SubnetId:           aws.String(record.SubnetId),
		VpcId:              aws.String(record.VpcId),
		AvailabilityZone:   aws.String(record.AvailabilityZone),
		PrivateIpAddress:   aws.String(record.PrivateIpAddress),
		MacAddress:         aws.String(record.MacAddress),
		Description:        aws.String(record.Description),
		Status:             aws.String(record.Status),
		OwnerId:            aws.String(accountID),
		RequesterManaged:   aws.Bool(requesterManaged),
		InterfaceType:      aws.String("interface"),
		SourceDestCheck:    aws.Bool(record.SourceDestCheck == nil || *record.SourceDestCheck),
		PrivateIpAddresses: []*ec2.NetworkInterfacePrivateIpAddress{
			{
				Primary:          aws.Bool(true),
				PrivateIpAddress: aws.String(record.PrivateIpAddress),
			},
		},
		Groups: []*ec2.GroupIdentifier{},
	}

	if len(record.SecurityGroupIds) > 0 {
		groups := make([]*ec2.GroupIdentifier, 0, len(record.SecurityGroupIds))
		for _, sgId := range record.SecurityGroupIds {
			groups = append(groups, &ec2.GroupIdentifier{
				GroupId: aws.String(sgId),
			})
		}
		eni.Groups = groups
	}

	if record.PublicIpAddress != "" {
		eni.Association = &ec2.NetworkInterfaceAssociation{
			PublicIp: aws.String(record.PublicIpAddress),
		}
	}

	if record.AttachmentId != "" {
		eni.Attachment = &ec2.NetworkInterfaceAttachment{
			AttachmentId:        aws.String(record.AttachmentId),
			InstanceId:          aws.String(record.InstanceId),
			DeviceIndex:         aws.Int64(record.DeviceIndex),
			Status:              aws.String("attached"),
			DeleteOnTermination: aws.Bool(true),
		}
	}

	eni.TagSet = utils.MapToEC2Tags(record.Tags)

	return eni
}

// generateENIMac creates a locally-administered unicast MAC address from an ENI ID.
func generateENIMac(eniId string) string {
	return utils.HashMAC(eniId)
}

// portEventPayload is the wire shape for vpc.create-port / vpc.delete-port.
// Mirrors network/subscribers.PortEvent — duplicated here to avoid a
// subscribers → handlers import cycle.
type portEventPayload struct {
	NetworkInterfaceId string   `json:"network_interface_id"`
	SubnetId           string   `json:"subnet_id"`
	VpcId              string   `json:"vpc_id"`
	PrivateIpAddress   string   `json:"private_ip_address"`
	MacAddress         string   `json:"mac_address"`
	SecurityGroupIds   []string `json:"security_group_ids,omitempty"`
}

// publishPortEvent publishes a port lifecycle event fire-and-forget. Used for
// vpc.delete-port — failure on delete is harmless (the port is going away
// anyway and the reconciler converges any leftover OVN state).
func (s *VPCServiceImpl) publishPortEvent(topic, eniId, subnetId, vpcId, privateIP, macAddr string, sgIds []string) {
	utils.PublishEvent(s.natsConn, topic, portEventPayload{
		NetworkInterfaceId: eniId,
		SubnetId:           subnetId,
		VpcId:              vpcId,
		PrivateIpAddress:   privateIP,
		MacAddress:         macAddr,
		SecurityGroupIds:   sgIds,
	})
}

// requestPortEvent sends a port lifecycle event via request-reply so vpcd
// OVSDB failures surface to the caller rather than being swallowed.
func (s *VPCServiceImpl) requestPortEvent(topic, eniId, subnetId, vpcId, privateIP, macAddr string, sgIds []string) error {
	return utils.RequestEvent(s.natsConn, topic, portEventPayload{
		NetworkInterfaceId: eniId,
		SubnetId:           subnetId,
		VpcId:              vpcId,
		PrivateIpAddress:   privateIP,
		MacAddress:         macAddr,
		SecurityGroupIds:   sgIds,
	}, vpcdSGEventTimeout)
}

// requestUpdatePortSGsEvent sends a vpc.update-port-sgs event to vpcd via
// request-reply. Errors surface to the caller; vpcd computes the OVN diff.
func (s *VPCServiceImpl) requestUpdatePortSGsEvent(eniId, privateIP string, sgIds []string) error {
	return utils.RequestEvent(s.natsConn, "vpc.update-port-sgs", struct {
		NetworkInterfaceId string   `json:"network_interface_id"`
		PrivateIpAddress   string   `json:"private_ip_address"`
		SecurityGroupIds   []string `json:"security_group_ids"`
	}{
		NetworkInterfaceId: eniId,
		PrivateIpAddress:   privateIP,
		SecurityGroupIds:   sgIds,
	}, vpcdSGEventTimeout)
}

// NATEvent represents a NAT lifecycle event published to NATS.
type NATEvent struct {
	VpcId      string `json:"vpc_id"`
	ExternalIP string `json:"external_ip"`
	LogicalIP  string `json:"logical_ip"`
	PortName   string `json:"port_name"`
	MAC        string `json:"mac"`
}

// publishNATEvent publishes a NAT lifecycle event (vpc.add-nat or vpc.delete-nat) to NATS.
func (s *VPCServiceImpl) publishNATEvent(topic, vpcId, externalIP, logicalIP, portName, mac string) {
	utils.PublishEvent(s.natsConn, topic, NATEvent{
		VpcId: vpcId, ExternalIP: externalIP, LogicalIP: logicalIP, PortName: portName, MAC: mac,
	})
}

// validateSGAttachment validates SG IDs before any KV write. Returns
// InvalidGroup.NotFound, InvalidParameterValue, SecurityGroupsPerInterfaceLimitExceeded,
// or MissingParameter per AWS contract.
const sgPerInterfaceLimit = 5

func (s *VPCServiceImpl) validateSGAttachment(accountID string, sgIds []string, vpcId string) error {
	if len(sgIds) == 0 {
		return errors.New(awserrors.ErrorMissingParameter)
	}
	if len(sgIds) > sgPerInterfaceLimit {
		return errors.New(awserrors.ErrorSecurityGroupsPerInterfaceLimitExceeded)
	}

	if err := s.requireVPCExists(accountID, vpcId); err != nil {
		return err
	}

	// Each SG must exist in the caller's account and belong to the same VPC.
	for _, sgId := range sgIds {
		sgEntry, err := s.sgKV.Get(utils.AccountKey(accountID, sgId))
		if err != nil {
			return errors.New(awserrors.ErrorInvalidGroupNotFound)
		}
		var sg SecurityGroupRecord
		if err := json.Unmarshal(sgEntry.Value(), &sg); err != nil {
			return errors.New(awserrors.ErrorServerInternal)
		}
		if sg.VpcId != vpcId {
			return errors.New(awserrors.ErrorInvalidParameterValue)
		}
	}
	return nil
}

// isEIPOwned checks whether the given ENI's public IP is owned by an Elastic IP.
// Returns (true, nil) if an EIP record references this ENI, (false, nil) if none
// match, or (false, err) if the KV store could not be read.
func (s *VPCServiceImpl) isEIPOwned(eniId, accountID string) (bool, error) {
	if s.eipKV == nil {
		return false, nil
	}
	keys, err := s.eipKV.Keys()
	if err != nil {
		if errors.Is(err, nats.ErrNoKeysFound) {
			return false, nil
		}
		return false, fmt.Errorf("eipKV.Keys: %w", err)
	}
	prefix := accountID + "."
	for _, k := range keys {
		if len(k) < len(prefix) || k[:len(prefix)] != prefix {
			continue
		}
		entry, err := s.eipKV.Get(k)
		if err != nil {
			return false, fmt.Errorf("eipKV.Get(%s): %w", k, err)
		}
		var record struct {
			ENIId string `json:"eni_id"`
		}
		if err := json.Unmarshal(entry.Value(), &record); err != nil {
			slog.Warn("isEIPOwned: malformed EIP record", "key", k, "err", err)
			continue
		}
		if record.ENIId == eniId {
			return true, nil
		}
	}
	return false, nil
}
