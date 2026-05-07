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
	"github.com/mulgadc/spinifex/spinifex/utils"
	"github.com/nats-io/nats.go"
)

const (
	KVBucketENIs        = "spinifex-vpc-enis"
	KVBucketENIsVersion = 1
)

// ENIRecord represents a stored Elastic Network Interface
type ENIRecord struct {
	NetworkInterfaceId string            `json:"network_interface_id"`
	SubnetId           string            `json:"subnet_id"`
	VpcId              string            `json:"vpc_id"`
	AvailabilityZone   string            `json:"availability_zone"`
	PrivateIpAddress   string            `json:"private_ip_address"`
	MacAddress         string            `json:"mac_address"`
	Description        string            `json:"description"`
	Status             string            `json:"status"` // available, in-use, attaching, detaching
	AttachmentId       string            `json:"attachment_id,omitempty"`
	InstanceId         string            `json:"instance_id,omitempty"`
	DeviceIndex        int64             `json:"device_index"`
	PublicIpAddress    string            `json:"public_ip_address,omitempty"` // Auto-assigned or EIP
	PublicIpPool       string            `json:"public_ip_pool,omitempty"`    // Pool name the public IP came from
	SecurityGroupIds   []string          `json:"security_group_ids,omitempty"`
	Tags               map[string]string `json:"tags"`
	CreatedAt          time.Time         `json:"created_at"`
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

	// Allocate IP from subnet
	var privateIP string
	if input.PrivateIpAddress != nil && *input.PrivateIpAddress != "" {
		// TODO: validate the requested IP is in the subnet range and not already allocated
		privateIP = *input.PrivateIpAddress
	} else {
		ip, err := s.ipam.AllocateIP(subnetId, subnet.CidrBlock)
		if err != nil {
			return nil, errors.New(awserrors.ErrorServerInternal)
		}
		privateIP = ip
	}

	eniId := utils.GenerateResourceID("eni")

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

	// Publish vpc.create-port event for vpcd topology translation. The SG
	// list flows through the event so vpcd joins the LSP to its port groups
	// in the same OVSDB transaction as the LSP create — Phase 4.4 of the SG
	// enforcement plan.
	s.publishPortEvent("vpc.create-port", eniId, subnetId, subnet.VpcId, privateIP, macAddr, sgIdsIn)

	return &ec2.CreateNetworkInterfaceOutput{
		NetworkInterface: s.eniRecordToEC2(&record, accountID),
	}, nil
}

// DeleteNetworkInterface deletes an ENI
func (s *VPCServiceImpl) DeleteNetworkInterface(input *ec2.DeleteNetworkInterfaceInput, accountID string) (*ec2.DeleteNetworkInterfaceOutput, error) {
	if input.NetworkInterfaceId == nil || *input.NetworkInterfaceId == "" {
		return nil, errors.New(awserrors.ErrorMissingParameter)
	}

	eniId := *input.NetworkInterfaceId
	key := utils.AccountKey(accountID, eniId)

	// Get the ENI record
	entry, err := s.eniKV.Get(key)
	if err != nil {
		return nil, errors.New(awserrors.ErrorInvalidNetworkInterfaceIDNotFound)
	}

	var record ENIRecord
	if err := json.Unmarshal(entry.Value(), &record); err != nil {
		return nil, errors.New(awserrors.ErrorServerInternal)
	}

	// Cannot delete an in-use ENI
	if record.Status == "in-use" {
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
			portName := "port-" + eniId
			s.publishNATEvent("vpc.delete-nat", record.VpcId, record.PublicIpAddress, record.PrivateIpAddress, portName, record.MacAddress)
			if err := s.externalIPAM.ReleaseIP(record.PublicIpPool, record.PublicIpAddress); err != nil {
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
	if len(input.Groups) == 0 && input.Description == nil {
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
		SourceDestCheck:    aws.Bool(true),
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

// publishPortEvent publishes a port lifecycle event (vpc.create-port /
// vpc.delete-port) to NATS for vpcd's TopologyHandler. sgIds carries the SG
// membership the LSP should join atomically with the create, or the
// (informational) membership at delete.
func (s *VPCServiceImpl) publishPortEvent(topic, eniId, subnetId, vpcId, privateIP, macAddr string, sgIds []string) {
	utils.PublishEvent(s.natsConn, topic, struct {
		NetworkInterfaceId string   `json:"network_interface_id"`
		SubnetId           string   `json:"subnet_id"`
		VpcId              string   `json:"vpc_id"`
		PrivateIpAddress   string   `json:"private_ip_address"`
		MacAddress         string   `json:"mac_address"`
		SecurityGroupIds   []string `json:"security_group_ids,omitempty"`
	}{
		NetworkInterfaceId: eniId,
		SubnetId:           subnetId,
		VpcId:              vpcId,
		PrivateIpAddress:   privateIP,
		MacAddress:         macAddr,
		SecurityGroupIds:   sgIds,
	})
}

// requestUpdatePortSGsEvent sends a vpc.update-port-sgs event to vpcd via
// request-reply so OVN port-group reconciliation errors surface to the caller
// instead of being swallowed. The payload is declarative — vpcd reads its
// libovsdb cache to compute the diff.
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

// validateSGAttachment is the boundary validator shared by RunInstances (via
// CreateNetworkInterface), CreateNetworkInterface itself, and
// ModifyNetworkInterfaceAttribute. It runs *before* any KV write so failures
// don't leave half-state.
//
// AWS contract reproduced here:
//   - InvalidGroup.NotFound when an SG ID is unknown to the account.
//   - InvalidParameterValue when an SG's VPC differs from the resolved VPC.
//   - SecurityGroupsPerInterfaceLimitExceeded when >5 SGs (AWS default).
//   - MissingParameter when the list is empty (every ENI must have ≥1 SG).
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
