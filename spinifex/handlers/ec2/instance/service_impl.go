package handlers_ec2_instance

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	"github.com/mulgadc/spinifex/spinifex/config"
	"github.com/mulgadc/spinifex/spinifex/filterutil"
	"github.com/mulgadc/spinifex/spinifex/gpu"
	handlers_dns "github.com/mulgadc/spinifex/spinifex/handlers/dns"
	handlers_ec2_vpc "github.com/mulgadc/spinifex/spinifex/handlers/ec2/vpc"
	"github.com/mulgadc/spinifex/spinifex/instancetypes"
	"github.com/mulgadc/spinifex/spinifex/network/topology"
	"github.com/mulgadc/spinifex/spinifex/objectstore"
	spxtypes "github.com/mulgadc/spinifex/spinifex/types"
	"github.com/mulgadc/spinifex/spinifex/utils"
	"github.com/mulgadc/spinifex/spinifex/vm"
	"github.com/mulgadc/viperblock/types"
	"github.com/mulgadc/viperblock/viperblock"
	"github.com/mulgadc/viperblock/viperblock/backends/s3"
	"github.com/nats-io/nats.go"
)

// VolumeInfo holds volume information returned from GenerateVolumes
// for populating BlockDeviceMappings in the EC2 API response.
type VolumeInfo struct {
	VolumeId            string
	DeviceName          string
	AttachTime          time.Time
	DeleteOnTermination bool
}

// volumeParams holds parsed block device mapping parameters for volume creation.
type volumeParams struct {
	size                int
	deviceName          string
	volumeType          string
	iops                int
	imageId             string
	snapshotId          string
	deleteOnTermination bool
}

// floorVolumeSizeToAMI ensures the requested size is at least the AMI's snapshot
// size. Under-sizing hangs the guest in dracut; we round up silently rather than
// returning InvalidParameterValue, since a slightly larger volume is less harmful.
func floorVolumeSizeToAMI(ctx context.Context, loader AMIMetaLoader, imageID string, requested int) int {
	if loader == nil || !strings.HasPrefix(imageID, "ami-") {
		return requested
	}
	amiMeta, err := loader.GetAMIConfig(ctx, imageID)
	if err != nil {
		slog.WarnContext(ctx, "floorVolumeSizeToAMI: AMI metadata fetch failed; using requested size — guest may dracut-hang if requested is smaller than the AMI snapshot", "imageId", imageID, "err", err)
		return requested
	}
	if amiMeta.VolumeSizeGiB == 0 {
		return requested
	}
	amiSize := utils.SafeUint64ToInt64(amiMeta.VolumeSizeGiB) * 1024 * 1024 * 1024
	if amiSize <= int64(requested) {
		return requested
	}
	slog.InfoContext(ctx, "floorVolumeSizeToAMI: rounding up to AMI snapshot size", "imageId", imageID, "requestedBytes", requested, "amiBytes", amiSize)
	return int(amiSize)
}

// parseVolumeParams extracts volume parameters from RunInstancesInput,
// applying defaults and resolving AMI-based image IDs.
func parseVolumeParams(input *ec2.RunInstancesInput) volumeParams {
	p := volumeParams{
		size:                4 * 1024 * 1024 * 1024, // 4GB default
		deviceName:          "/dev/vda",
		deleteOnTermination: true, // matches AWS RunInstances behavior
	}

	if len(input.BlockDeviceMappings) > 0 {
		bdm := input.BlockDeviceMappings[0]
		if bdm.DeviceName != nil {
			p.deviceName = *bdm.DeviceName
		}
		if bdm.Ebs != nil {
			if bdm.Ebs.VolumeSize != nil {
				p.size = int(*bdm.Ebs.VolumeSize) * 1024 * 1024 * 1024
			}
			if bdm.Ebs.VolumeType != nil {
				p.volumeType = *bdm.Ebs.VolumeType
			}
			if bdm.Ebs.Iops != nil {
				p.iops = int(*bdm.Ebs.Iops)
			}
			if bdm.Ebs.DeleteOnTermination != nil {
				p.deleteOnTermination = *bdm.Ebs.DeleteOnTermination
			}
		}
	}

	if strings.HasPrefix(*input.ImageId, "ami-") {
		p.imageId = utils.GenerateResourceID("vol")
		p.snapshotId = *input.ImageId
	} else {
		p.imageId = *input.ImageId
	}

	return p
}

var _ InstanceService = (*InstanceServiceImpl)(nil)

// InstanceServiceImpl handles daemon-side EC2 instance operations.
type InstanceServiceImpl struct {
	config            *config.Config
	instanceTypes     map[string]*ec2.InstanceTypeInfo
	natsConn          *nats.Conn
	objectStore       objectstore.ObjectStore
	vmMgr             *vm.Manager
	resourceMgr       InstanceTypeAllocator
	stoppedStore      StoppedInstanceStore
	volumeDeleter     VolumeDeleter
	eniDeleter        ENIDeleter
	ipReleaser        PublicIPReleaser
	tagWriter         InstanceTagWriter
	gpuClaimer        GPUClaimer
	amiLoader         AMIMetaLoader
	keyValidator      KeyPairValidator
	eniCreator        ENICreator
	ipAllocator       PublicIPAllocator
	dnsBaseDomain     string
	dnsInternalDomain string
}

// NewInstanceServiceImpl creates a new instance service implementation for daemon use.
func NewInstanceServiceImpl(
	cfg *config.Config,
	instanceTypes map[string]*ec2.InstanceTypeInfo,
	nc *nats.Conn,
	store objectstore.ObjectStore,
	vmMgr *vm.Manager,
	resourceMgr InstanceTypeAllocator,
	stoppedStore StoppedInstanceStore,
) *InstanceServiceImpl {
	return &InstanceServiceImpl{
		config:            cfg,
		instanceTypes:     instanceTypes,
		natsConn:          nc,
		objectStore:       store,
		vmMgr:             vmMgr,
		resourceMgr:       resourceMgr,
		stoppedStore:      stoppedStore,
		dnsBaseDomain:     handlers_dns.ResolveBaseDomain(cfg),
		dnsInternalDomain: handlers_dns.ResolveInternalDomain(cfg),
	}
}

// SetTerminationDeps wires the dependencies required by the terminate paths.
// Kept separate from the main constructor so handlers needing only read or
// modify paths can construct a service without dragging the VPC/volume stack in.
func (s *InstanceServiceImpl) SetTerminationDeps(vd VolumeDeleter, ed ENIDeleter, pr PublicIPReleaser, tw InstanceTagWriter) {
	s.volumeDeleter = vd
	s.eniDeleter = ed
	s.ipReleaser = pr
	s.tagWriter = tw
}

// SetGPUClaimer wires the GPU passthrough claim/release dependency used by
// StartStoppedInstance. nil disables GPU passthrough for the service.
func (s *InstanceServiceImpl) SetGPUClaimer(g GPUClaimer) {
	s.gpuClaimer = g
}

// SetRunInstancesDeps wires the AMI/key/VPC/IPAM dependencies required by
// RunInstances. Kept separate from the constructor so handlers needing only
// read paths can build the service without dragging the full stack in.
func (s *InstanceServiceImpl) SetRunInstancesDeps(ami AMIMetaLoader, key KeyPairValidator, eni ENICreator, ipam PublicIPAllocator) {
	s.amiLoader = ami
	s.keyValidator = key
	s.eniCreator = eni
	s.ipAllocator = ipam
}

// RunInstance creates a single EC2 instance (called per-instance by daemon)
// Returns the VM struct and EC2 instance metadata.
func (s *InstanceServiceImpl) RunInstance(input *ec2.RunInstancesInput) (*vm.VM, *ec2.Instance, error) {
	// Validate instance type exists
	_, exists := s.instanceTypes[*input.InstanceType]
	if !exists {
		return nil, nil, errors.New(awserrors.ErrorInvalidInstanceType)
	}

	instanceId := utils.GenerateResourceID("i")

	// Create new instance structure
	instance := &vm.VM{
		ID:           instanceId,
		Status:       vm.StateProvisioning,
		InstanceType: *input.InstanceType,
	}

	// Create EC2 instance metadata
	ec2Instance := &ec2.Instance{
		State: &ec2.InstanceState{},
	}
	ec2Instance.SetInstanceId(instance.ID)
	ec2Instance.SetInstanceType(*input.InstanceType)
	if input.ImageId != nil {
		ec2Instance.SetImageId(*input.ImageId)
	}
	if input.KeyName != nil {
		ec2Instance.SetKeyName(*input.KeyName)
	}
	ec2Instance.SetLaunchTime(time.Now())
	ec2Instance.State.SetCode(0)
	ec2Instance.State.SetName("pending")

	// Project the instance type's architecture onto the customer instance so it
	// flows to DescribeInstances and the IMDS identity document for free.
	if arch := instanceArchitecture(s.instanceTypes[*input.InstanceType]); arch != "" {
		ec2Instance.SetArchitecture(arch)
	}

	// Stamp the constant IMDSv2-only block so DescribeInstances reports the
	// posture; only the hop limit is request-driven.
	var hopLimit *int64
	if input.MetadataOptions != nil {
		hopLimit = input.MetadataOptions.HttpPutResponseHopLimit
	}
	ec2Instance.MetadataOptions = buildMetadataOptions(hopLimit)

	// IAM instance profile attached at launch: gateway has already resolved
	// the reference to a canonical ARN and enforced iam:PassRole; here we
	// just persist it on the VM and generate the association ID. Id is left
	// for the gateway to enrich on DescribeInstances since daemons have no
	// IAM service access.
	if input.IamInstanceProfile != nil {
		arn := aws.StringValue(input.IamInstanceProfile.Arn)
		if arn != "" {
			instance.IamInstanceProfileArn = arn
			instance.IamInstanceProfileAssociationId = utils.GenerateResourceID("iip-assoc")
			ec2Instance.IamInstanceProfile = &ec2.IamInstanceProfile{
				Arn: aws.String(arn),
			}
		}
	}

	// Apply instance-scoped tags from TagSpecifications so DescribeInstances
	// returns them and tag filters match. Node groups discover their workers by
	// the spinifex:eks-cluster tag; scale-up convergence depends on it.
	ec2Instance.Tags = instanceTagsFromSpec(input.TagSpecifications)

	// Store EC2 API metadata in VM for DescribeInstances compatibility
	instance.RunInstancesInput = input
	instance.Instance = ec2Instance

	return instance, ec2Instance, nil
}

// instanceTagsFromSpec extracts the instance-scoped tags from a RunInstances
// TagSpecifications list. Only ResourceType "instance" applies to the launched
// instance; volume/network-interface specs are handled elsewhere.
func instanceTagsFromSpec(specs []*ec2.TagSpecification) []*ec2.Tag {
	var tags []*ec2.Tag
	for _, spec := range specs {
		if spec == nil || aws.StringValue(spec.ResourceType) != "instance" {
			continue
		}
		for _, t := range spec.Tags {
			if t == nil || t.Key == nil {
				continue
			}
			tags = append(tags, &ec2.Tag{Key: t.Key, Value: t.Value})
		}
	}
	return tags
}

// ApplyInstanceTagMutation applies a set or remove tag mutation to a tag list,
// mirroring the central tag store's merge, value-match delete, and clear-all
// semantics, and returns the resulting list sorted by key.
func ApplyInstanceTagMutation(existing []*ec2.Tag, data *spxtypes.InstanceTagsData, remove bool) []*ec2.Tag {
	tags := make(map[string]string, len(existing))
	for _, t := range existing {
		if t == nil || t.Key == nil {
			continue
		}
		tags[aws.StringValue(t.Key)] = aws.StringValue(t.Value)
	}

	switch {
	case remove && data == nil:
		utils.ApplyTagRemovals(tags, nil, nil)
	case remove:
		utils.ApplyTagRemovals(tags, data.TagKeys, data.Tags)
	case data != nil:
		maps.Copy(tags, data.Tags)
	}

	out := make([]*ec2.Tag, 0, len(tags))
	for key, value := range tags {
		out = append(out, &ec2.Tag{Key: aws.String(key), Value: aws.String(value)})
	}
	sort.Slice(out, func(i, j int) bool { return aws.StringValue(out[i].Key) < aws.StringValue(out[j].Key) })
	return out
}

// TagsToMap converts a record tag list to the central tag store's map form,
// skipping nil entries and nil keys.
func TagsToMap(tags []*ec2.Tag) map[string]string {
	out := make(map[string]string, len(tags))
	for _, t := range tags {
		if t == nil || t.Key == nil {
			continue
		}
		out[aws.StringValue(t.Key)] = aws.StringValue(t.Value)
	}
	return out
}

// WriteInstanceTags applies the tag mutation to the instance record (the source
// of truth) and writes the resulting full tag set to the central tag store, so
// both stores move together. Callers own record persistence and must not hold
// the vm.Manager lock, since the central write is an S3 round-trip.
func WriteInstanceTags(ctx context.Context, instance *vm.VM, data *spxtypes.InstanceTagsData, remove bool, central InstanceTagWriter, accountID string) error {
	if instance == nil || instance.Instance == nil || central == nil {
		return errors.New(awserrors.ErrorServerInternal)
	}
	instance.Instance.Tags = ApplyInstanceTagMutation(instance.Instance.Tags, data, remove)
	return central.PutResourceTags(ctx, accountID, instance.ID, TagsToMap(instance.Instance.Tags))
}

// TagStoppedInstance applies a create-tags/delete-tags mutation to a stopped
// instance's shared KV record and the central tag store together. A missing or
// cross-account record returns InvalidID.NotFound and writes nothing.
func (s *InstanceServiceImpl) TagStoppedInstance(ctx context.Context, instanceID string, data *spxtypes.InstanceTagsData, remove bool, central InstanceTagWriter, accountID string) error {
	if s.stoppedStore == nil {
		slog.ErrorContext(ctx, "TagStoppedInstance: stopped store not available")
		return errors.New(awserrors.ErrorServerInternal)
	}

	instance, err := s.stoppedStore.LoadStoppedInstance(instanceID)
	if err != nil {
		slog.ErrorContext(ctx, "TagStoppedInstance: failed to load stopped instance", "instanceId", instanceID, "err", err)
		return errors.New(awserrors.ErrorServerInternal)
	}
	if instance == nil {
		return errors.New(awserrors.ErrorInvalidInstanceIDNotFound)
	}
	if !IsInstanceVisible(accountID, instance.AccountID) {
		slog.WarnContext(ctx, "TagStoppedInstance: instance not visible",
			"instanceId", instanceID, "callerAccount", accountID, "ownerAccount", instance.AccountID)
		return errors.New(awserrors.ErrorInvalidInstanceIDNotFound)
	}
	if instance.Instance == nil {
		slog.ErrorContext(ctx, "TagStoppedInstance: instance.Instance is nil, data integrity issue", "instanceId", instanceID)
		return errors.New(awserrors.ErrorServerInternal)
	}

	if err := WriteInstanceTags(ctx, instance, data, remove, central, accountID); err != nil {
		slog.ErrorContext(ctx, "TagStoppedInstance: central tag store write failed", "instanceId", instanceID, "err", err)
		return errors.New(awserrors.ErrorServerInternal)
	}

	// CAS the tag mutation into the shared KV record instead of a wholesale
	// WriteStoppedInstance: createIfAbsent=false means a claim that deletes
	// the record between the Load above and this write fails cleanly with
	// nats.ErrKeyNotFound rather than resurrecting a stale stopped entry.
	newTags := instance.Instance.Tags
	if _, err := s.stoppedStore.UpdateStoppedInstance(instanceID, func(v *vm.VM) {
		if v.Instance != nil {
			v.Instance.Tags = newTags
		}
	}); err != nil {
		if errors.Is(err, nats.ErrKeyNotFound) {
			slog.WarnContext(ctx, "TagStoppedInstance: instance claimed concurrently, not resurrecting", "instanceId", instanceID)
			return errors.New(awserrors.ErrorInvalidInstanceIDNotFound)
		}
		slog.ErrorContext(ctx, "TagStoppedInstance: failed to write stopped instance", "instanceId", instanceID, "err", err)
		return errors.New(awserrors.ErrorServerInternal)
	}
	return nil
}

// deleteCentralInstanceTags removes a terminated instance's central tag store
// entry so describe-tags stops reporting it, while the terminated record keeps
// its tags until TTL. Best-effort: the terminate already succeeded, so a
// failed delete is logged rather than surfaced.
func (s *InstanceServiceImpl) deleteCentralInstanceTags(ctx context.Context, instanceID, ownerAccountID string) {
	if s.tagWriter == nil || ownerAccountID == "" {
		return
	}
	if err := s.tagWriter.DeleteAllTags(ctx, ownerAccountID, instanceID); err != nil {
		slog.ErrorContext(ctx, "deleteCentralInstanceTags: central tag store delete failed",
			"instanceId", instanceID, "err", err)
	}
}

// PrepareRunInstances validates input, allocates capacity, creates VM metadata,
// auto-creates the primary ENI, and auto-assigns a public IP when needed.
// Does NOT touch vmMgr or NATS — callers insert VMs then call LaunchRunInstances.
//
// A non-empty reservationID makes this a targeted launch into an On-Demand
// Capacity Reservation: capacity is consumed from that reservation (net-zero
// swap), the count is capped at the reservation's free slots (never spilling
// onto general capacity), and every rollback site returns slots to the
// reservation rather than the general pool.
func (s *InstanceServiceImpl) PrepareRunInstances(ctx context.Context, input *ec2.RunInstancesInput, accountID, reservationID string) (*ec2.Reservation, []*vm.VM, *ec2.InstanceTypeInfo, error) {
	if accountID == "" {
		return nil, nil, nil, errors.New(awserrors.ErrorServerInternal)
	}
	if input == nil || input.InstanceType == nil {
		return nil, nil, nil, errors.New(awserrors.ErrorMissingParameter)
	}

	// Reject a metadata-options downgrade before allocating capacity: http-tokens=
	// optional and other weakenings return UnsupportedOperation under IMDSv2
	// enforcement. RunInstance seeds the hop limit onto the per-instance block.
	if opts := input.MetadataOptions; opts != nil {
		if err := validateMetadataOptions(
			aws.StringValue(opts.HttpTokens),
			aws.StringValue(opts.HttpEndpoint),
			aws.StringValue(opts.HttpProtocolIpv6),
			aws.StringValue(opts.InstanceMetadataTags),
			opts.HttpPutResponseHopLimit,
		); err != nil {
			return nil, nil, nil, err
		}
	}

	instanceType, exists := s.instanceTypes[*input.InstanceType]
	if !exists {
		slog.ErrorContext(ctx, "PrepareRunInstances: invalid instance type", "InstanceType", *input.InstanceType)
		return nil, nil, nil, errors.New(awserrors.ErrorInvalidInstanceType)
	}

	if input.ImageId == nil || *input.ImageId == "" {
		slog.ErrorContext(ctx, "PrepareRunInstances: missing ImageId")
		return nil, nil, nil, errors.New(awserrors.ErrorMissingParameter)
	}
	if s.amiLoader == nil {
		slog.ErrorContext(ctx, "PrepareRunInstances: AMI loader not initialized")
		return nil, nil, nil, errors.New(awserrors.ErrorServerInternal)
	}
	amiMeta, err := s.amiLoader.GetAMIConfig(ctx, *input.ImageId)
	if err != nil {
		slog.ErrorContext(ctx, "PrepareRunInstances: AMI not found", "imageId", *input.ImageId, "err", err)
		return nil, nil, nil, errors.New(awserrors.ErrorInvalidAMIIDNotFound)
	}
	// Caller must own the AMI or the owner alias must be non-account-ID (e.g. "self", "spinifex", "").
	amiOwner := amiMeta.ImageOwnerAlias
	if amiOwner != "" && amiOwner != accountID && utils.IsAccountID(amiOwner) {
		slog.WarnContext(ctx, "PrepareRunInstances: AMI not owned by caller", "imageId", *input.ImageId, "amiOwner", amiOwner, "accountID", accountID)
		return nil, nil, nil, errors.New(awserrors.ErrorInvalidAMIIDNotFound)
	}

	if input.KeyName != nil && *input.KeyName != "" {
		if s.keyValidator == nil {
			slog.ErrorContext(ctx, "PrepareRunInstances: key validator not initialized")
			return nil, nil, nil, errors.New(awserrors.ErrorServerInternal)
		}
		if err := s.keyValidator.ValidateKeyPairExists(ctx, accountID, *input.KeyName); err != nil {
			slog.ErrorContext(ctx, "PrepareRunInstances: key pair not found", "keyName", *input.KeyName, "err", err)
			return nil, nil, nil, errors.New(awserrors.ErrorInvalidKeyPairNotFound)
		}
	}

	minCount := int(*input.MinCount)
	maxCount := int(*input.MaxCount)

	var allocatableCount int
	if reservationID == "" {
		allocatableCount = s.resourceMgr.CanAllocate(instanceType, maxCount)
	} else {
		// Targeted launch: confined to the reservation. Cap at its free slots so
		// the overflow never spills onto the node's general capacity.
		allocatableCount = min(s.resourceMgr.ReservationAvailable(reservationID, accountID, instanceType), maxCount)
	}
	if allocatableCount < minCount {
		errCode := awserrors.ErrorInsufficientInstanceCapacity
		if reservationID != "" {
			errCode = awserrors.ErrorReservationCapacityExceeded
		}
		slog.ErrorContext(ctx, "PrepareRunInstances: insufficient capacity", "requested", minCount, "available", allocatableCount, "InstanceType", *input.InstanceType, "reservationId", reservationID)
		return nil, nil, nil, errors.New(errCode)
	}

	launchCount := allocatableCount
	slog.InfoContext(ctx, "PrepareRunInstances: count determined", "min", minCount, "max", maxCount, "launching", launchCount)

	// Each Allocate is atomic; under contention we may get fewer than
	// allocatableCount — the < minCount branch below rolls back.
	var allocatedCount int
	for i := 0; i < launchCount; i++ {
		var allocErr error
		if reservationID == "" {
			allocErr = s.resourceMgr.Allocate(instanceType)
		} else {
			allocErr = s.resourceMgr.AllocateFromReservation(reservationID, accountID, instanceType)
		}
		if allocErr != nil {
			slog.ErrorContext(ctx, "PrepareRunInstances: allocate failed mid-allocation", "allocated", allocatedCount, "err", allocErr)
			break
		}
		allocatedCount++
	}
	if allocatedCount < minCount {
		for i := 0; i < allocatedCount; i++ {
			if reservationID == "" {
				s.resourceMgr.Deallocate(instanceType)
			} else {
				s.resourceMgr.ReleaseToReservation(reservationID, instanceType)
			}
		}
		errCode := awserrors.ErrorInsufficientInstanceCapacity
		if reservationID != "" {
			errCode = awserrors.ErrorReservationCapacityExceeded
		}
		slog.ErrorContext(ctx, "PrepareRunInstances: insufficient capacity after allocation", "allocated", allocatedCount, "minCount", minCount)
		return nil, nil, nil, errors.New(errCode)
	}
	launchCount = allocatedCount

	var instances []*vm.VM
	var allEC2Instances []*ec2.Instance
	var lastRunErr error

	for i := 0; i < launchCount; i++ {
		instance, ec2Instance, err := s.RunInstance(input)
		if err != nil {
			slog.ErrorContext(ctx, "PrepareRunInstances: RunInstance failed", "index", i, "err", err)
			lastRunErr = err
			if reservationID == "" {
				s.resourceMgr.Deallocate(instanceType)
			} else {
				s.resourceMgr.ReleaseToReservation(reservationID, instanceType)
			}
			continue
		}
		instance.BootMode = amiMeta.BootMode

		// Terraform may pass subnet/SG via NetworkInterfaces[0]; lift to top-level.
		if (input.SubnetId == nil || *input.SubnetId == "") &&
			len(input.NetworkInterfaces) > 0 && input.NetworkInterfaces[0] != nil {
			nic := input.NetworkInterfaces[0]
			if nic.SubnetId != nil && *nic.SubnetId != "" {
				input.SubnetId = nic.SubnetId
			}
			if len(input.SecurityGroupIds) == 0 && len(nic.Groups) > 0 {
				input.SecurityGroupIds = nic.Groups
			}
		}

		// Pre-created ENI: attach the existing ENI instead of auto-creating one
		// (AWS parity; used by EKS launcher to pre-create the ENI in customer VPC).
		preExistingENIID := ""
		if len(input.NetworkInterfaces) > 0 && input.NetworkInterfaces[0] != nil {
			preExistingENIID = aws.StringValue(input.NetworkInterfaces[0].NetworkInterfaceId)
		}
		if preExistingENIID != "" {
			if s.eniCreator == nil {
				slog.ErrorContext(ctx, "PrepareRunInstances: pre-created ENI specified but ENI service unavailable",
					"instanceId", instance.ID, "eniId", preExistingENIID)
				lastRunErr = errors.New(awserrors.ErrorServerInternal)
				if reservationID == "" {
					s.resourceMgr.Deallocate(instanceType)
				} else {
					s.resourceMgr.ReleaseToReservation(reservationID, instanceType)
				}
				continue
			}
			if err := s.attachPreCreatedENI(ctx, accountID, preExistingENIID, instance, ec2Instance); err != nil {
				slog.ErrorContext(ctx, "PrepareRunInstances: pre-created ENI attach failed",
					"instanceId", instance.ID, "eniId", preExistingENIID, "err", err)
				lastRunErr = err
				if reservationID == "" {
					s.resourceMgr.Deallocate(instanceType)
				} else {
					s.resourceMgr.ReleaseToReservation(reservationID, instanceType)
				}
				continue
			}
			ec2Instance.SetAmiLaunchIndex(int64(len(allEC2Instances)))
			instances = append(instances, instance)
			allEC2Instances = append(allEC2Instances, ec2Instance)
			continue
		}

		if (input.SubnetId == nil || *input.SubnetId == "") && s.eniCreator != nil {
			defaultSubnet, dsErr := s.eniCreator.GetDefaultSubnet(ctx, accountID)
			if dsErr == nil && defaultSubnet != nil {
				input.SubnetId = aws.String(defaultSubnet.SubnetID)
				slog.InfoContext(ctx, "PrepareRunInstances: resolved default subnet", "instanceId", instance.ID, "subnetId", defaultSubnet.SubnetID)
			}
		}

		if input.SubnetId != nil && *input.SubnetId != "" && s.eniCreator != nil {
			eniOut, eniErr := s.eniCreator.CreateNetworkInterface(ctx, &ec2.CreateNetworkInterfaceInput{
				SubnetId:    input.SubnetId,
				Description: aws.String("Primary network interface for " + instance.ID),
				Groups:      input.SecurityGroupIds,
			}, accountID)
			if eniErr != nil {
				slog.ErrorContext(ctx, "PrepareRunInstances: auto-create ENI failed", "instanceId", instance.ID, "subnetId", *input.SubnetId, "err", eniErr)
				lastRunErr = eniErr
				if reservationID == "" {
					s.resourceMgr.Deallocate(instanceType)
				} else {
					s.resourceMgr.ReleaseToReservation(reservationID, instanceType)
				}
				continue
			}

			eni := eniOut.NetworkInterface
			instance.ENIId = *eni.NetworkInterfaceId
			instance.ENIMac = *eni.MacAddress

			if _, attachErr := s.eniCreator.AttachENI(ctx, accountID, instance.ENIId, instance.ID, 0); attachErr != nil {
				// Without the attachment RegisterTargets silently drops the target;
				// aborting prevents a leak of the auto-assigned EIP.
				slog.ErrorContext(ctx, "PrepareRunInstances: AttachENI failed — aborting launch",
					"eniId", instance.ENIId, "instanceId", instance.ID, "err", attachErr)
				if _, delErr := s.eniDeleter.DeleteNetworkInterface(ctx, &ec2.DeleteNetworkInterfaceInput{
					NetworkInterfaceId: &instance.ENIId,
				}, accountID); delErr != nil {
					slog.WarnContext(ctx, "PrepareRunInstances: failed to delete auto-created ENI after attach failure",
						"eniId", instance.ENIId, "err", delErr)
				}
				lastRunErr = attachErr
				if reservationID == "" {
					s.resourceMgr.Deallocate(instanceType)
				} else {
					s.resourceMgr.ReleaseToReservation(reservationID, instanceType)
				}
				continue
			}
			ec2Instance.SetPrivateIpAddress(*eni.PrivateIpAddress)
			ec2Instance.SetSubnetId(*input.SubnetId)
			ec2Instance.SetVpcId(*eni.VpcId)
			ec2Instance.SecurityGroups = eni.Groups
			ec2Instance.NetworkInterfaces = []*ec2.InstanceNetworkInterface{
				{
					NetworkInterfaceId: eni.NetworkInterfaceId,
					PrivateIpAddress:   eni.PrivateIpAddress,
					MacAddress:         eni.MacAddress,
					SubnetId:           input.SubnetId,
					VpcId:              eni.VpcId,
					Status:             aws.String("in-use"),
					Groups:             eni.Groups,
					Attachment: &ec2.InstanceNetworkInterfaceAttachment{
						DeviceIndex: aws.Int64(0),
						Status:      aws.String("attached"),
					},
				},
			}

			slog.InfoContext(ctx, "PrepareRunInstances: auto-created ENI for VPC instance",
				"instanceId", instance.ID,
				"eniId", instance.ENIId,
				"privateIp", *eni.PrivateIpAddress,
				"mac", instance.ENIMac,
			)

			if s.ipAllocator != nil {
				subnet, subErr := s.eniCreator.GetSubnet(ctx, accountID, *input.SubnetId)
				wantPublic := subErr == nil && subnet != nil && subnet.MapPublicIpOnLaunch
				if len(input.NetworkInterfaces) > 0 && input.NetworkInterfaces[0] != nil && input.NetworkInterfaces[0].AssociatePublicIpAddress != nil {
					wantPublic = *input.NetworkInterfaces[0].AssociatePublicIpAddress
				}
				if wantPublic {
					region := s.config.Region
					az := s.config.AZ
					publicIP, poolName, allocErr := s.ipAllocator.AllocateIP(ctx, region, az, handlers_ec2_vpc.PurposeENIPublic, "", *eni.NetworkInterfaceId, instance.ID)
					if allocErr != nil {
						// Fail rather than boot an unreachable instance;
						// detach before delete since in-use ENIs reject deletion.
						slog.ErrorContext(ctx, "PrepareRunInstances: public IP allocation failed — aborting launch",
							"instanceId", instance.ID, "eniId", *eni.NetworkInterfaceId, "err", allocErr)
						if detErr := s.eniCreator.DetachENI(ctx, accountID, *eni.NetworkInterfaceId); detErr != nil {
							slog.WarnContext(ctx, "PrepareRunInstances: failed to detach ENI after public-IP allocation failure",
								"eniId", *eni.NetworkInterfaceId, "err", detErr)
						}
						if _, delErr := s.eniDeleter.DeleteNetworkInterface(ctx, &ec2.DeleteNetworkInterfaceInput{
							NetworkInterfaceId: eni.NetworkInterfaceId,
						}, accountID); delErr != nil {
							slog.WarnContext(ctx, "PrepareRunInstances: failed to delete ENI after public-IP allocation failure",
								"eniId", *eni.NetworkInterfaceId, "err", delErr)
						}
						lastRunErr = errors.New(awserrors.ErrorInsufficientAddressCapacity)
						if reservationID == "" {
							s.resourceMgr.Deallocate(instanceType)
						} else {
							s.resourceMgr.ReleaseToReservation(reservationID, instanceType)
						}
						continue
					} else {
						if updateErr := s.eniCreator.UpdateENIPublicIP(ctx, accountID, *eni.NetworkInterfaceId, publicIP, poolName); updateErr != nil {
							slog.WarnContext(ctx, "PrepareRunInstances: failed to update ENI with public IP", "eniId", *eni.NetworkInterfaceId, "err", updateErr)
						}
						portName := topology.Port(*eni.NetworkInterfaceId)
						if natErr := utils.AddNAT(s.natsConn, *eni.VpcId, publicIP, *eni.PrivateIpAddress, portName, *eni.MacAddress); natErr != nil {
							slog.ErrorContext(ctx, "PrepareRunInstances: vpc.add-nat failed — rolling back public IP to avoid surfacing an unreachable address",
								"instanceId", instance.ID, "publicIp", publicIP, "pool", poolName, "err", natErr)
							// Neutralise before releasing in case timeout committed the rule.
							utils.PublishNATEvent(s.natsConn, "vpc.delete-nat", *eni.VpcId, publicIP, *eni.PrivateIpAddress, portName, *eni.MacAddress)
							s.rollbackAutoAssignedPublicIP(ctx, accountID, instance.ID, *eni.NetworkInterfaceId, publicIP, poolName)
							lastRunErr = natErr
							if reservationID == "" {
								s.resourceMgr.Deallocate(instanceType)
							} else {
								s.resourceMgr.ReleaseToReservation(reservationID, instanceType)
							}
							continue
						}
						ec2Instance.PublicIpAddress = aws.String(publicIP)
						instance.PublicIP = publicIP
						instance.PublicIPPool = poolName
						slog.InfoContext(ctx, "PrepareRunInstances: auto-assigned public IP",
							"instanceId", instance.ID,
							"publicIp", publicIP,
							"privateIp", *eni.PrivateIpAddress,
							"pool", poolName,
						)
					}
				}
			}
		}

		ec2Instance.SetAmiLaunchIndex(int64(len(allEC2Instances)))
		instances = append(instances, instance)
		allEC2Instances = append(allEC2Instances, ec2Instance)
	}

	if len(instances) < minCount {
		for range instances {
			if reservationID == "" {
				s.resourceMgr.Deallocate(instanceType)
			} else {
				s.resourceMgr.ReleaseToReservation(reservationID, instanceType)
			}
		}
		errCode := awserrors.ErrorServerInternal
		if lastRunErr != nil {
			if _, ok := awserrors.ErrorLookup[lastRunErr.Error()]; ok {
				errCode = lastRunErr.Error()
			}
		}
		slog.ErrorContext(ctx, "PrepareRunInstances: failed to create minimum instances", "created", len(instances), "minCount", minCount, "err", errCode)
		return nil, nil, nil, errors.New(errCode)
	}

	reservation := &ec2.Reservation{}
	reservation.SetReservationId(utils.GenerateResourceID("r"))
	reservation.SetOwnerId(accountID)
	reservation.Instances = allEC2Instances

	for _, instance := range instances {
		instance.Reservation = reservation
		instance.AccountID = accountID
		if reservationID != "" {
			instance.CapacityReservationId = reservationID
		}
		if input.Placement != nil && input.Placement.GroupName != nil && *input.Placement.GroupName != "" {
			instance.PlacementGroupName = *input.Placement.GroupName
		}
	}

	return reservation, instances, instanceType, nil
}

// publishDNS registers (or withdraws) the public + private A records for a batch
// of instances with the control-plane DNS writer. Best-effort: a failure is
// logged by the writer helper and never blocks the lifecycle operation; the
// reconcile loop repairs any miss. No-op when northstar is not configured.
func (s *InstanceServiceImpl) publishDNS(accountID string, action handlers_dns.Action, instances []*vm.VM) {
	if s.dnsBaseDomain == "" || len(instances) == 0 {
		return
	}
	var changes []handlers_dns.Change
	for _, instance := range instances {
		privateIP := ""
		if instance.Instance != nil && instance.Instance.PrivateIpAddress != nil {
			privateIP = *instance.Instance.PrivateIpAddress
		}
		changes = append(changes, handlers_dns.EC2Changes(action, s.config.Region, s.dnsBaseDomain, s.dnsInternalDomain, instance.PublicIP, privateIP)...)
	}
	handlers_dns.PublishChangesBestEffort(s.natsConn, accountID, changes)
}

// attachPreCreatedENI verifies the ENI is available, attaches it as device-0,
// and populates the VM + ec2.Instance. Public-IP auto-assignment is skipped;
// the caller manages that out-of-band.
func (s *InstanceServiceImpl) attachPreCreatedENI(ctx context.Context, accountID, eniID string, instance *vm.VM, ec2Instance *ec2.Instance) error {
	eniInfo, err := s.eniCreator.GetENI(ctx, accountID, eniID)
	if err != nil {
		return err
	}
	if eniInfo == nil {
		return errors.New(awserrors.ErrorInvalidNetworkInterfaceIDNotFound)
	}
	if eniInfo.Status == "in-use" {
		return errors.New(awserrors.ErrorInvalidNetworkInterfaceInUse)
	}
	if _, err := s.eniCreator.AttachENI(ctx, accountID, eniID, instance.ID, 0); err != nil {
		return err
	}

	instance.ENIId = eniID
	instance.ENIMac = eniInfo.MacAddress

	ec2Instance.SetPrivateIpAddress(eniInfo.PrivateIpAddress)
	ec2Instance.SetSubnetId(eniInfo.SubnetID)
	ec2Instance.SetVpcId(eniInfo.VpcID)
	if len(eniInfo.SecurityGroupIDs) > 0 {
		groups := make([]*ec2.GroupIdentifier, 0, len(eniInfo.SecurityGroupIDs))
		for _, id := range eniInfo.SecurityGroupIDs {
			groups = append(groups, &ec2.GroupIdentifier{GroupId: aws.String(id)})
		}
		ec2Instance.SecurityGroups = groups
	}
	ec2Instance.NetworkInterfaces = []*ec2.InstanceNetworkInterface{{
		NetworkInterfaceId: aws.String(eniID),
		PrivateIpAddress:   aws.String(eniInfo.PrivateIpAddress),
		MacAddress:         aws.String(eniInfo.MacAddress),
		SubnetId:           aws.String(eniInfo.SubnetID),
		VpcId:              aws.String(eniInfo.VpcID),
		Status:             aws.String("in-use"),
		Attachment: &ec2.InstanceNetworkInterfaceAttachment{
			DeviceIndex: aws.Int64(0),
			Status:      aws.String("attached"),
		},
	}}

	slog.InfoContext(ctx, "PrepareRunInstances: attached pre-created ENI",
		"instanceId", instance.ID,
		"eniId", eniID,
		"privateIp", eniInfo.PrivateIpAddress,
		"mac", eniInfo.MacAddress,
	)
	return nil
}

// LaunchRunInstances runs the heavyweight launch loop for VMs already inserted
// into vmMgr: volume prep, GPU claim, vmMgr.Run. Partial failures are tolerated.
func (s *InstanceServiceImpl) LaunchRunInstances(ctx context.Context, instances []*vm.VM, input *ec2.RunInstancesInput, instanceType *ec2.InstanceTypeInfo) {
	var successCount int
	launchedByAccount := make(map[string][]*vm.VM)
	for _, instance := range instances {
		// Skip if a concurrent terminate raced with prepare.
		status := s.vmMgr.Status(instance)
		if status != vm.StatePending && status != vm.StateProvisioning {
			slog.InfoContext(ctx, "LaunchRunInstances: instance state changed during provisioning, skipping launch",
				"instanceId", instance.ID, "status", string(status))
			continue
		}

		// Pre-compute dev MAC for per-interface cloud-init netplan (route suppression).
		if s.config.Daemon.DevNetworking && instance.ENIId != "" {
			instance.DevMAC = vm.GenerateDevMAC(instance.ID)
		}

		volumeInfos, err := s.GenerateVolumes(ctx, input, instance)
		if err != nil {
			slog.ErrorContext(ctx, "LaunchRunInstances: GenerateVolumes failed", "instanceId", instance.ID, "err", err)
			s.vmMgr.MarkFailed(ctx, instance, "volume_preparation_failed")
			continue
		}

		instance.Instance.BlockDeviceMappings = make([]*ec2.InstanceBlockDeviceMapping, 0, len(volumeInfos))
		for _, vi := range volumeInfos {
			mapping := &ec2.InstanceBlockDeviceMapping{}
			mapping.SetDeviceName(vi.DeviceName)
			mapping.Ebs = &ec2.EbsInstanceBlockDevice{}
			mapping.Ebs.SetVolumeId(vi.VolumeId)
			mapping.Ebs.SetAttachTime(vi.AttachTime)
			mapping.Ebs.SetDeleteOnTermination(vi.DeleteOnTermination)
			mapping.Ebs.SetStatus("attached")
			instance.Instance.BlockDeviceMappings = append(instance.Instance.BlockDeviceMappings, mapping)
		}

		if s.gpuClaimer != nil && instancetypes.IsGPUType(instanceType) {
			profileName := instancetypes.MIGProfileFromType(aws.StringValue(instanceType.InstanceType))
			att, gpuErr := s.gpuClaimer.Claim(instance.ID, profileName)
			if gpuErr != nil {
				slog.ErrorContext(ctx, "LaunchRunInstances: GPU claim failed", "instanceId", instance.ID, "err", gpuErr)
				s.vmMgr.MarkFailed(ctx, instance, "gpu_claim_failed")
				continue
			}
			instance.GPUAttachments = []gpu.GPUAttachment{*att}
			slog.InfoContext(ctx, "LaunchRunInstances: GPU claimed for instance", "instanceId", instance.ID,
				"pci", att.PCIAddress, "mdev", att.MdevPath)
		}

		if err := s.vmMgr.Run(ctx, instance); err != nil {
			slog.ErrorContext(ctx, "LaunchRunInstances: vmMgr.Run failed", "instanceId", instance.ID, "err", err)
			if len(instance.GPUAttachments) > 0 && s.gpuClaimer != nil {
				if releaseErr := s.gpuClaimer.Release(instance.ID); releaseErr != nil {
					slog.ErrorContext(ctx, "LaunchRunInstances: GPU release failed after launch failure",
						"instanceId", instance.ID, "err", releaseErr)
				}
			}
			s.vmMgr.MarkFailed(ctx, instance, "launch_failed")
			continue
		}

		if s.vmMgr.Status(instance) != vm.StateRunning {
			slog.InfoContext(ctx, "LaunchRunInstances: launch did not reach running state", "instanceId", instance.ID)
			continue
		}
		s.vmMgr.UpdateGuestDeviceNames(instance)

		successCount++
		launchedByAccount[instance.AccountID] = append(launchedByAccount[instance.AccountID], instance)
		slog.InfoContext(ctx, "LaunchRunInstances: launched instance", "instanceId", instance.ID)
	}

	withdrawByAccount := make(map[string][]*vm.VM)
	for accountID, launched := range launchedByAccount {
		s.publishDNS(accountID, handlers_dns.ActionUpsert, launched)
		for _, instance := range launched {
			if s.terminateRacedLaunch(instance) {
				withdrawByAccount[accountID] = append(withdrawByAccount[accountID], instance)
			}
		}
	}
	for accountID, instances := range withdrawByAccount {
		s.publishDNS(accountID, handlers_dns.ActionDelete, instances)
	}
	slog.InfoContext(ctx, "LaunchRunInstances: completed", "requested", len(instances), "launched", successCount)
}

func needsDNSWithdrawal(status vm.InstanceState) bool {
	return status != vm.StateRunning && status != vm.StateStopping && status != vm.StateStopped
}

// terminateRacedLaunch reports whether a terminate raced the deferred launch
// UPSERT: it reads the terminate flag stamped under the lock (plus status) since
// the async state transition may not have landed, which a status-only check misses.
func (s *InstanceServiceImpl) terminateRacedLaunch(instance *vm.VM) bool {
	withdraw := false
	s.vmMgr.Inspect(instance, func(v *vm.VM) {
		withdraw = v.Attributes.TerminateInstance || needsDNSWithdrawal(v.Status)
	})
	return withdraw
}

// RunInstances is for non-daemon callers (tests). The daemon calls
// PrepareRunInstances + LaunchRunInstances directly to respond before launching.
func (s *InstanceServiceImpl) RunInstances(ctx context.Context, input *ec2.RunInstancesInput, accountID string) (*ec2.Reservation, error) {
	reservation, instances, instanceType, err := s.PrepareRunInstances(ctx, input, accountID, "")
	if err != nil {
		return nil, err
	}
	for _, instance := range instances {
		s.vmMgr.Insert(instance)
	}
	s.LaunchRunInstances(ctx, instances, input, instanceType)
	return reservation, nil
}

// RebootInstance handles an ec2.cmd reboot for a running instance on this node.
func (s *InstanceServiceImpl) RebootInstance(ctx context.Context, instance *vm.VM, command spxtypes.EC2InstanceCommand) error {
	slog.InfoContext(ctx, "RebootInstance: rebooting instance", "id", command.ID)

	if err := s.vmMgr.Reboot(ctx, instance.ID); err != nil {
		switch {
		case errors.Is(err, vm.ErrInstanceNotFound):
			return errors.New(awserrors.ErrorInvalidInstanceIDNotFound)
		case errors.Is(err, vm.ErrInvalidTransition):
			slog.ErrorContext(ctx, "RebootInstance: instance not in running state",
				"instanceId", command.ID, "err", err)
			return errors.New(awserrors.ErrorIncorrectInstanceState)
		default:
			slog.ErrorContext(ctx, "RebootInstance: reboot failed", "instanceId", command.ID, "err", err)
			return errors.New(awserrors.ErrorServerInternal)
		}
	}

	slog.InfoContext(ctx, "RebootInstance: rebooted", "instanceId", command.ID)
	return nil
}

// StartInstance handles an ec2.cmd start for a locally stopped instance.
func (s *InstanceServiceImpl) StartInstance(ctx context.Context, instance *vm.VM, command spxtypes.EC2InstanceCommand) error {
	slog.InfoContext(ctx, "StartInstance: starting instance", "id", command.ID)

	status := s.vmMgr.Status(instance)
	// StateError is startable so a crashed instance can be manually recovered.
	if status != vm.StateStopped && status != vm.StateError {
		slog.ErrorContext(ctx, "StartInstance: instance not in a startable state", "instanceId", command.ID, "status", status)
		return errors.New(awserrors.ErrorIncorrectInstanceState)
	}

	instanceType, ok := s.resourceMgr.InstanceTypes()[instance.InstanceType]
	if ok {
		if err := s.resourceMgr.Allocate(instanceType); err != nil {
			slog.ErrorContext(ctx, "StartInstance: failed to allocate resources", "id", command.ID, "err", err)
			return errors.New(awserrors.ErrorInsufficientInstanceCapacity)
		}
	}

	// Clear stop attribute before launch; a stale StopInstance=true would
	// cause the daemon to skip QEMU reconnect on restart.
	s.vmMgr.UpdateState(instance.ID, func(v *vm.VM) { v.Attributes = command.Attributes })

	if err := s.vmMgr.Start(ctx, instance.ID); err != nil {
		if ok {
			s.resourceMgr.Deallocate(instanceType)
		}
		switch {
		case errors.Is(err, vm.ErrInstanceNotFound):
			slog.WarnContext(ctx, "StartInstance: instance not found in manager",
				"instanceId", command.ID, "err", err)
			return errors.New(awserrors.ErrorInvalidInstanceIDNotFound)
		default:
			slog.ErrorContext(ctx, "StartInstance: vmMgr.Start failed", "err", err)
			return errors.New(awserrors.ErrorServerInternal)
		}
	}

	s.vmMgr.UpdateGuestDeviceNames(instance)

	slog.InfoContext(ctx, "StartInstance: started", "instanceId", instance.ID)
	return nil
}

// StopOrTerminateInstance handles an ec2.cmd stop or terminate. Validates the
// transition synchronously, then dispatches Stop/Terminate in a goroutine.
func (s *InstanceServiceImpl) StopOrTerminateInstance(ctx context.Context, instance *vm.VM, command spxtypes.EC2InstanceCommand) error {
	isTerminate := command.Attributes.TerminateInstance
	action := "Stopping"
	initialState := vm.StateStopping
	if isTerminate {
		action = "Terminating"
		initialState = vm.StateShuttingDown
	}

	slog.InfoContext(ctx, "StopOrTerminateInstance: "+action, "id", command.ID)

	// Single lock acquisition covers protection check, idempotency, transition
	// validation, and attribute stamp to prevent races with ModifyInstanceAttribute.
	var (
		protected, raced, stateMismatch bool
		currentState                    vm.InstanceState
	)
	ok := s.vmMgr.UpdateState(instance.ID, func(v *vm.VM) {
		currentState = v.Status
		if isTerminate && v.IsTerminationProtected() {
			protected = true
			return
		}
		if isTerminate && v.Status == vm.StateShuttingDown {
			raced = true
			return
		}
		if !vm.IsValidTransition(v.Status, initialState) {
			stateMismatch = true
			return
		}
		v.Attributes = command.Attributes
		// Auto-disassociate IAM profile on terminate (AWS parity; stop/start preserves it).
		// Done under the same lock so DescribeInstances never sees a terminated
		// instance advertising a profile.
		if isTerminate && v.IamInstanceProfileArn != "" {
			slog.InfoContext(ctx, "IAM instance profile auto-disassociated",
				"instance_id", v.ID,
				"association_id", v.IamInstanceProfileAssociationId,
				"profile_arn", v.IamInstanceProfileArn,
				"reason", "instance terminated")
			v.IamInstanceProfileArn = ""
			v.IamInstanceProfileAssociationId = ""
		}
	})
	if !ok {
		if isTerminate {
			// Idempotent terminate (rule #1): the instance was reclaimed/torn down
			// between resolve and lock, so it is already gone.
			slog.InfoContext(ctx, "StopOrTerminateInstance: instance already gone, terminate is idempotent",
				"instanceId", instance.ID)
			return nil
		}
		slog.WarnContext(ctx, "StopOrTerminateInstance: instance no longer in running map",
			"instanceId", instance.ID)
		return errors.New(awserrors.ErrorInvalidInstanceIDNotFound)
	}
	if protected {
		slog.WarnContext(ctx, "StopOrTerminateInstance: instance has termination protection",
			"instanceId", instance.ID)
		return errors.New(awserrors.ErrorOperationNotPermitted)
	}
	if raced {
		// Idempotent: a concurrent terminate is already in progress.
		slog.InfoContext(ctx, "StopOrTerminateInstance: instance already shutting down, terminate is idempotent", "instanceId", instance.ID)
		return nil
	}
	if stateMismatch {
		// Surface IncorrectInstanceState synchronously (AWS SDK expects 400).
		slog.WarnContext(ctx, "StopOrTerminateInstance: instance in incorrect state for "+strings.ToLower(action),
			"instanceId", instance.ID, "currentState", string(currentState))
		return errors.New(awserrors.ErrorIncorrectInstanceState)
	}

	// Withdraw the instance's DNS records on terminate; stop/start retains
	// the IP and the name, so they are a no-op here.
	if isTerminate {
		s.publishDNS(instance.AccountID, handlers_dns.ActionDelete, []*vm.VM{instance})
	}

	go func(id, ownerAccountID string) { //nolint:gosec // detached lifecycle op: stop/terminate continues after the API ack; request ctx would cancel it
		var err error
		if isTerminate {
			err = s.vmMgr.Terminate(id)
		} else {
			err = s.vmMgr.Stop(id)
		}
		if err != nil {
			if errors.Is(err, vm.ErrInvalidTransition) {
				slog.Warn("StopOrTerminateInstance: lifecycle transition raced; ack already sent",
					"id", id, "action", strings.ToLower(action), "err", err)
				return
			}
			slog.Error("StopOrTerminateInstance: failed to "+strings.ToLower(action), "err", err, "id", id)
			return
		}
		if isTerminate {
			s.deleteCentralInstanceTags(context.Background(), id, ownerAccountID)
		}
	}(instance.ID, instance.AccountID)

	return nil
}

// AssociateIamInstanceProfile attaches an instance profile to a running instance.
// Validates no existing profile, then atomically writes the ARN + new association ID.
// InstanceProfile.Id is left nil; the gateway enriches it from IAMService.
func (s *InstanceServiceImpl) AssociateIamInstanceProfile(ctx context.Context, instance *vm.VM, command spxtypes.EC2InstanceCommand) (*ec2.IamInstanceProfileAssociation, error) {
	if command.IamProfileAssociationData == nil || command.IamProfileAssociationData.InstanceProfileArn == "" {
		return nil, errors.New(awserrors.ErrorMissingParameter)
	}
	profileArn := command.IamProfileAssociationData.InstanceProfileArn
	newID := utils.GenerateResourceID("iip-assoc")
	timestamp := time.Now().UTC()

	var alreadyAssociated bool
	found, err := s.vmMgr.UpdateAndPersist(instance.ID, func(v *vm.VM) bool {
		if v.IamInstanceProfileArn != "" {
			alreadyAssociated = true
			return false
		}
		v.IamInstanceProfileArn = profileArn
		v.IamInstanceProfileAssociationId = newID
		return true
	})
	if err != nil {
		slog.ErrorContext(ctx, "AssociateIamInstanceProfile: persist failed", "instanceId", instance.ID, "err", err)
		return nil, errors.New(awserrors.ErrorServerInternal)
	}
	if alreadyAssociated {
		return nil, errors.New(awserrors.ErrorIamInstanceProfileAlreadyAssociated)
	}
	if !found {
		return nil, errors.New(awserrors.ErrorInvalidInstanceIDNotFound)
	}

	slog.InfoContext(ctx, "AssociateIamInstanceProfile: associated",
		"instanceId", instance.ID, "associationId", newID, "profileArn", profileArn)
	return &ec2.IamInstanceProfileAssociation{
		AssociationId:      aws.String(newID),
		InstanceId:         aws.String(instance.ID),
		IamInstanceProfile: &ec2.IamInstanceProfile{Arn: aws.String(profileArn)},
		State:              aws.String(ec2.IamInstanceProfileAssociationStateAssociated),
		Timestamp:          aws.Time(timestamp),
	}, nil
}

// DisassociateIamProfileAssociation clears the profile association on the local VM.
// Returns nil when no local VM owns the ID so the gateway fan-out can skip this node.
func (s *InstanceServiceImpl) DisassociateIamProfileAssociation(ctx context.Context, input *ec2.DisassociateIamInstanceProfileInput, accountID string) (*ec2.IamInstanceProfileAssociation, error) {
	if input == nil || input.AssociationId == nil || *input.AssociationId == "" {
		return nil, errors.New(awserrors.ErrorMissingParameter)
	}
	associationID := *input.AssociationId
	owner, ok := s.findInstanceByAssociationID(associationID, accountID)
	if !ok {
		return nil, nil
	}

	var clearedArn string
	timestamp := time.Now().UTC()
	_, err := s.vmMgr.UpdateAndPersist(owner, func(v *vm.VM) bool {
		// Re-check under lock: a concurrent terminate may have cleared the association.
		if v.IamInstanceProfileAssociationId != associationID {
			return false
		}
		clearedArn = v.IamInstanceProfileArn
		v.IamInstanceProfileArn = ""
		v.IamInstanceProfileAssociationId = ""
		return true
	})
	if err != nil {
		slog.ErrorContext(ctx, "DisassociateIamProfileAssociation: persist failed", "instanceId", owner, "err", err)
		return nil, errors.New(awserrors.ErrorServerInternal)
	}
	if clearedArn == "" {
		// Lost a race; NoOp so the gateway collector skips this node.
		return nil, nil
	}

	slog.InfoContext(ctx, "DisassociateIamProfileAssociation: disassociated",
		"instanceId", owner, "associationId", associationID, "profileArn", clearedArn)
	return &ec2.IamInstanceProfileAssociation{
		AssociationId:      aws.String(associationID),
		InstanceId:         aws.String(owner),
		IamInstanceProfile: &ec2.IamInstanceProfile{Arn: aws.String(clearedArn)},
		State:              aws.String(ec2.IamInstanceProfileAssociationStateDisassociating),
		Timestamp:          aws.Time(timestamp),
	}, nil
}

// ReplaceIamProfileAssociation atomically swaps the profile ARN and AssociationId.
// Returns nil when no local VM owns the old ID. The gateway has already resolved
// the ARN to canonical form.
func (s *InstanceServiceImpl) ReplaceIamProfileAssociation(ctx context.Context, input *ec2.ReplaceIamInstanceProfileAssociationInput, accountID string) (*ec2.IamInstanceProfileAssociation, error) {
	if input == nil || input.AssociationId == nil || *input.AssociationId == "" ||
		input.IamInstanceProfile == nil || aws.StringValue(input.IamInstanceProfile.Arn) == "" {
		return nil, errors.New(awserrors.ErrorMissingParameter)
	}
	oldID := *input.AssociationId
	newArn := *input.IamInstanceProfile.Arn

	owner, ok := s.findInstanceByAssociationID(oldID, accountID)
	if !ok {
		return nil, nil
	}

	newID := utils.GenerateResourceID("iip-assoc")
	timestamp := time.Now().UTC()
	var swapped bool
	_, err := s.vmMgr.UpdateAndPersist(owner, func(v *vm.VM) bool {
		if v.IamInstanceProfileAssociationId != oldID {
			return false
		}
		v.IamInstanceProfileArn = newArn
		v.IamInstanceProfileAssociationId = newID
		swapped = true
		return true
	})
	if err != nil {
		slog.ErrorContext(ctx, "ReplaceIamProfileAssociation: persist failed", "instanceId", owner, "err", err)
		return nil, errors.New(awserrors.ErrorServerInternal)
	}
	if !swapped {
		return nil, nil
	}

	slog.InfoContext(ctx, "ReplaceIamProfileAssociation: replaced",
		"instanceId", owner, "oldAssociationId", oldID, "newAssociationId", newID, "profileArn", newArn)
	return &ec2.IamInstanceProfileAssociation{
		AssociationId:      aws.String(newID),
		InstanceId:         aws.String(owner),
		IamInstanceProfile: &ec2.IamInstanceProfile{Arn: aws.String(newArn)},
		State:              aws.String(ec2.IamInstanceProfileAssociationStateAssociated),
		Timestamp:          aws.Time(timestamp),
	}, nil
}

// DescribeIamProfileAssociations returns live associations on this node visible to
// the caller. Supports "instance-id" and "state" filters; unknown filters return
// InvalidParameterValue.
func (s *InstanceServiceImpl) DescribeIamProfileAssociations(ctx context.Context, input *ec2.DescribeIamInstanceProfileAssociationsInput, accountID string) (*ec2.DescribeIamInstanceProfileAssociationsOutput, error) {
	assocFilter := stringPtrSliceToSet(input.AssociationIds)
	instFilter := make(map[string]bool)
	stateFilter := make(map[string]bool)
	for _, f := range input.Filters {
		if f == nil || f.Name == nil {
			continue
		}
		switch *f.Name {
		case "instance-id":
			for _, v := range f.Values {
				if v != nil && *v != "" {
					instFilter[*v] = true
				}
			}
		case "state":
			for _, v := range f.Values {
				if v != nil && *v != "" {
					stateFilter[*v] = true
				}
			}
		default:
			return nil, errors.New(awserrors.ErrorInvalidParameterValue)
		}
	}

	out := &ec2.DescribeIamInstanceProfileAssociationsOutput{}
	s.vmMgr.ForEach(func(v *vm.VM) {
		if v.IamInstanceProfileArn == "" || v.IamInstanceProfileAssociationId == "" {
			return
		}
		if !IsInstanceVisible(accountID, v.AccountID) {
			return
		}
		if len(assocFilter) > 0 && !assocFilter[v.IamInstanceProfileAssociationId] {
			return
		}
		if len(instFilter) > 0 && !instFilter[v.ID] {
			return
		}
		// Only "associated" state exists; other state filters yield empty results.
		const liveState = ec2.IamInstanceProfileAssociationStateAssociated
		if len(stateFilter) > 0 && !stateFilter[liveState] {
			return
		}
		out.IamInstanceProfileAssociations = append(out.IamInstanceProfileAssociations, &ec2.IamInstanceProfileAssociation{
			AssociationId:      aws.String(v.IamInstanceProfileAssociationId),
			InstanceId:         aws.String(v.ID),
			IamInstanceProfile: &ec2.IamInstanceProfile{Arn: aws.String(v.IamInstanceProfileArn)},
			State:              aws.String(liveState),
		})
	})
	return out, nil
}

// findInstanceByAssociationID returns the VM ID for the given AssociationId
// if the caller's account owns it. Mutations re-validate under UpdateAndPersist.
func (s *InstanceServiceImpl) findInstanceByAssociationID(associationID, accountID string) (string, bool) {
	var owner string
	s.vmMgr.ForEach(func(v *vm.VM) {
		if owner != "" || v.IamInstanceProfileAssociationId != associationID {
			return
		}
		if !IsInstanceVisible(accountID, v.AccountID) {
			return
		}
		owner = v.ID
	})
	if owner == "" {
		return "", false
	}
	return owner, true
}

func stringPtrSliceToSet(in []*string) map[string]bool {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]bool, len(in))
	for _, s := range in {
		if s != nil && *s != "" {
			out[*s] = true
		}
	}
	return out
}

func (s *InstanceServiceImpl) GenerateVolumes(ctx context.Context, input *ec2.RunInstancesInput, instance *vm.VM) ([]VolumeInfo, error) {
	p := parseVolumeParams(input)
	if input.ImageId != nil {
		p.size = floorVolumeSizeToAMI(ctx, s.amiLoader, *input.ImageId, p.size)
	}

	// Capture attach time for the root volume
	attachTime := time.Now()

	volumeConfig := viperblock.VolumeConfig{
		VolumeMetadata: viperblock.VolumeMetadata{
			VolumeID:            p.imageId,
			SizeGiB:             utils.SafeIntToUint64(p.size / 1024 / 1024 / 1024),
			CreatedAt:           attachTime,
			DeviceName:          p.deviceName,
			VolumeType:          p.volumeType,
			IOPS:                p.iops,
			SnapshotID:          p.snapshotId,
			DeleteOnTermination: p.deleteOnTermination,
			TenantID:            instance.AccountID,
		},
	}

	size := p.size
	imageId := p.imageId
	deviceName := p.deviceName
	deleteOnTermination := p.deleteOnTermination

	// Step 1: Create or validate root volume
	err := s.prepareRootVolume(ctx, input, imageId, size, volumeConfig, instance, deleteOnTermination)
	if err != nil {
		return nil, err
	}

	// Step 2: Create EFI variable store (only when the AMI is UEFI; BIOS
	// guests must not allocate an orphan VARS volume).
	if instance.BootMode == "uefi" || instance.BootMode == "uefi-preferred" {
		arch := instanceArchitecture(s.instanceTypes[*input.InstanceType])
		err = s.prepareEFIVolume(ctx, imageId, volumeConfig, instance, arch)
		if err != nil {
			return nil, err
		}
	}

	// Return volume info for the root volume only (EFI is internal)
	volumeInfos := []VolumeInfo{
		{
			VolumeId:            imageId,
			DeviceName:          deviceName,
			AttachTime:          attachTime,
			DeleteOnTermination: deleteOnTermination,
		},
	}

	return volumeInfos, nil
}

// newViperblock creates a viperblock instance with the service's S3/Predastore credentials.
func (s *InstanceServiceImpl) newViperblock(volumeName string, size int, volumeConfig viperblock.VolumeConfig) (*viperblock.VB, error) {
	cfg := s3.S3Config{
		VolumeName: volumeName,
		VolumeSize: utils.SafeIntToUint64(size),
		Bucket:     s.config.Predastore.Bucket,
		Region:     s.config.Predastore.Region,
		AccessKey:  s.config.Predastore.AccessKey,
		SecretKey:  s.config.Predastore.SecretKey,
		Host:       s.config.Predastore.Host,
	}

	mkey, err := utils.LoadViperblockMasterKey(s.config.Viperblock.EncryptionKeyFile)
	if err != nil {
		return nil, err
	}

	vbconfig := viperblock.VB{
		VolumeName:        volumeName,
		VolumeSize:        utils.SafeIntToUint64(size),
		BaseDir:           s.config.WalDir,
		Cache:             viperblock.Cache{Config: viperblock.CacheConfig{Size: 0}},
		VolumeConfig:      volumeConfig,
		MasterKey:         mkey,
		EncryptionEnabled: mkey != nil,
	}

	vb, err := viperblock.New(&vbconfig, "s3", cfg)
	return vb, err
}

// prepareRootVolume handles creation/cloning of the root volume.
func (s *InstanceServiceImpl) prepareRootVolume(ctx context.Context, input *ec2.RunInstancesInput, imageId string, size int, volumeConfig viperblock.VolumeConfig, instance *vm.VM, deleteOnTermination bool) error {
	vb, err := s.newViperblock(imageId, size, volumeConfig)
	if err != nil {
		slog.ErrorContext(ctx, "Failed to connect to Viperblock store", "err", err)
		return errors.New(awserrors.ErrorServerInternal)
	}
	defer func() {
		vb.StopChunkUploader()
		vb.StopWALSyncer()
	}()

	// Initialize the backend
	err = vb.Backend.Init()
	if err != nil {
		slog.ErrorContext(ctx, "Failed to initialize backend", "err", err)
		return errors.New(awserrors.ErrorServerInternal)
	}

	// Only os.ErrNotExist means "clone from AMI"; any other error must abort.
	// Treating a tamper/mismatch as missing would overwrite live volume state.
	_, err = vb.LoadStateRequest("")
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		slog.ErrorContext(ctx, "Failed to load root volume state from backend",
			"imageId", imageId, "err", err)
		return errors.New(awserrors.ErrorServerInternal)
	}
	if err != nil {
		slog.InfoContext(ctx, "Volume does not yet exist, creating from AMI ...")
		if err = s.cloneAMIToVolume(ctx, input, vb); err != nil {
			return err
		}
	}

	// Append root volume to instance
	instance.EBSRequests.Mu.Lock()
	instance.EBSRequests.Requests = append(instance.EBSRequests.Requests, spxtypes.EBSRequest{
		Name:                imageId,
		Boot:                true,
		DeleteOnTermination: deleteOnTermination,
	})
	instance.EBSRequests.Mu.Unlock()

	return nil
}

// cloneAMIToVolume creates a new volume from an AMI using snapshot-based
// zero-copy cloning. The destination volume points at the AMI's frozen block
// map and reads on-demand from the AMI's chunks (copy-on-write).
func (s *InstanceServiceImpl) cloneAMIToVolume(ctx context.Context, input *ec2.RunInstancesInput, destVb *viperblock.VB) error {
	amiConfig, err := s.amiLoader.GetAMIConfig(ctx, *input.ImageId)
	if err != nil {
		slog.ErrorContext(ctx, "Could not load AMI config", "imageId", *input.ImageId, "err", err)
		return errors.New(awserrors.ErrorInvalidAMIIDNotFound)
	}

	snapshotID := amiConfig.SnapshotID
	if snapshotID == "" {
		slog.ErrorContext(ctx, "AMI has no snapshot ID, cannot perform zero-copy clone", "imageId", *input.ImageId)
		return errors.New(awserrors.ErrorServerInternal)
	}

	slog.InfoContext(ctx, "Cloning AMI via snapshot", "imageId", *input.ImageId, "snapshotID", snapshotID)

	// Set up destination volume from the snapshot (zero-copy)
	err = destVb.OpenFromSnapshot(snapshotID)
	if err != nil {
		slog.ErrorContext(ctx, "Failed to open from snapshot", "snapshotID", snapshotID, "err", err)
		return errors.New(awserrors.ErrorServerInternal)
	}

	// Persist the snapshot relationship to the backend
	err = destVb.SaveState()
	if err != nil {
		slog.ErrorContext(ctx, "Failed to save state", "err", err)
		return errors.New(awserrors.ErrorServerInternal)
	}

	err = destVb.SaveBlockState()
	if err != nil {
		slog.ErrorContext(ctx, "Failed to save block state", "err", err)
		return errors.New(awserrors.ErrorServerInternal)
	}

	err = destVb.RemoveLocalFiles()
	if err != nil {
		slog.WarnContext(ctx, "Failed to remove local files", "err", err)
	}

	return nil
}

// prepareEFIVolume creates the per-VM EFI variable store, sized exactly to the
// firmware VARS template (pflash requires byte-exact size) and seeded from it.
// arch is "x86_64" | "arm64".
func (s *InstanceServiceImpl) prepareEFIVolume(ctx context.Context, imageId string, volumeConfig viperblock.VolumeConfig, instance *vm.VM, arch string) error {
	codePath, varsTemplate, varsSize, err := vm.FirmwarePaths(arch)
	if err != nil {
		slog.ErrorContext(ctx, "UEFI firmware not installed on this host", "arch", arch, "err", err)
		return errors.New(awserrors.ErrorServerInternal)
	}
	template, err := os.ReadFile(varsTemplate)
	if err != nil {
		slog.ErrorContext(ctx, "Failed to read VARS template", "path", varsTemplate, "err", err)
		return errors.New(awserrors.ErrorServerInternal)
	}
	if int64(len(template)) != varsSize {
		slog.ErrorContext(ctx, "VARS template size mismatch between stat and read", "path", varsTemplate, "statSize", varsSize, "readSize", len(template))
		return errors.New(awserrors.ErrorServerInternal)
	}
	slog.InfoContext(ctx, "Preparing EFI variable store", "arch", arch, "code", codePath, "varsTemplate", varsTemplate, "size", varsSize)

	efiVolumeName := fmt.Sprintf("%s-efi", imageId)
	efiVolumeConfig := volumeConfig
	efiVolumeConfig.VolumeMetadata.VolumeID = efiVolumeName
	// Zero SizeGiB prevents viperblock from rounding the EFI volume size
	// up to GiB boundaries; pflash rejects any size beyond the VARS region.
	efiVolumeConfig.VolumeMetadata.SizeGiB = 0

	efiVb, err := s.newViperblock(efiVolumeName, int(varsSize), efiVolumeConfig)
	if err != nil {
		slog.ErrorContext(ctx, "Could not create EFI viperblock", "err", err)
		return errors.New(awserrors.ErrorServerInternal)
	}

	slog.DebugContext(ctx, "Initializing EFI Viperblock store backend")
	if err := efiVb.Backend.Init(); err != nil {
		slog.ErrorContext(ctx, "Failed to initialize EFI Viperblock store backend", "err", err)
		return errors.New(awserrors.ErrorServerInternal)
	}

	// Only os.ErrNotExist means "seed from template"; other errors must abort.
	// Treating a transient failure as missing would clobber guest-set BootOrder.
	_, loadErr := efiVb.LoadStateRequest("")
	if loadErr != nil && !errors.Is(loadErr, os.ErrNotExist) {
		slog.ErrorContext(ctx, "Failed to load EFI volume state from backend", "name", efiVolumeName, "err", loadErr)
		return errors.New(awserrors.ErrorServerInternal)
	}
	if loadErr != nil {
		slog.InfoContext(ctx, "EFI volume does not yet exist, seeding from firmware VARS template", "name", efiVolumeName)

		var walErr error
		if efiVb.UseShardedWAL {
			walErr = efiVb.OpenShardedWAL()
		} else {
			walErr = efiVb.OpenWAL(&efiVb.WAL, fmt.Sprintf("%s/%s", efiVb.WAL.BaseDir, types.GetFilePath(types.FileTypeWALChunk, efiVb.WAL.WallNum.Load(), efiVb.GetVolume())))
		}
		if walErr != nil {
			slog.ErrorContext(ctx, "Failed to load WAL", "err", walErr)
			return errors.New(awserrors.ErrorServerInternal)
		}

		if err := efiVb.OpenWAL(&efiVb.BlockToObjectWAL, fmt.Sprintf("%s/%s", efiVb.WAL.BaseDir, types.GetFilePath(types.FileTypeWALBlock, efiVb.BlockToObjectWAL.WallNum.Load(), efiVb.GetVolume()))); err != nil {
			slog.ErrorContext(ctx, "Failed to load block WAL", "err", err)
			return errors.New(awserrors.ErrorServerInternal)
		}

		if err := efiVb.WriteAt(0, template); err != nil {
			slog.ErrorContext(ctx, "Failed to seed EFI volume with VARS template", "err", err)
			return errors.New(awserrors.ErrorServerInternal)
		}
		if err := efiVb.Flush(); err != nil {
			slog.ErrorContext(ctx, "Failed to flush EFI volume", "err", err)
			return errors.New(awserrors.ErrorServerInternal)
		}
	}

	// Close is the durability boundary; a partial VARS write causes pflash to refuse launch.
	if err := efiVb.Close(); err != nil {
		slog.ErrorContext(ctx, "Failed to close EFI Viperblock store", "err", err)
		return errors.New(awserrors.ErrorServerInternal)
	}
	if err := efiVb.RemoveLocalFiles(); err != nil {
		slog.ErrorContext(ctx, "Failed to remove local files", "err", err)
	}

	instance.EBSRequests.Mu.Lock()
	instance.EBSRequests.Requests = append(instance.EBSRequests.Requests, spxtypes.EBSRequest{
		Name: efiVb.VolumeName,
		Boot: false,
		EFI:  true,
	})
	instance.EBSRequests.Mu.Unlock()

	return nil
}

// instanceArchitecture returns the AWS architecture string from the instance type.
// Returns "" on nil/malformed spec; the caller's firmware probe surfaces the error.
func instanceArchitecture(it *ec2.InstanceTypeInfo) string {
	if it == nil || it.ProcessorInfo == nil || len(it.ProcessorInfo.SupportedArchitectures) == 0 || it.ProcessorInfo.SupportedArchitectures[0] == nil {
		return ""
	}
	return *it.ProcessorInfo.SupportedArchitectures[0]
}

// DescribeInstancesValidFilters lists the filter names accepted by DescribeInstances
// (and the stopped/terminated variants, which share the same filter shape).
var DescribeInstancesValidFilters = map[string]bool{
	"instance-state-name": true,
	"instance-id":         true,
	"instance-type":       true,
	"vpc-id":              true,
	"subnet-id":           true,
	"tag-key":             true,
	"tag-value":           true,
}

// DescribeInstanceStatusValidFilters lists the filter names accepted by
// DescribeInstanceStatus. event.* and status.* filters are excluded as
// Mulga's health model has a single static value per status field.
var DescribeInstanceStatusValidFilters = map[string]bool{
	"availability-zone":   true,
	"instance-state-code": true,
	"instance-state-name": true,
}

// IsInstanceVisible reports whether the caller can see this instance.
// Pre-Phase4 instances (empty AccountID) are only visible to root (GlobalAccountID).
func IsInstanceVisible(callerAccountID, ownerAccountID string) bool {
	if ownerAccountID == "" {
		return callerAccountID == utils.GlobalAccountID
	}
	return callerAccountID == ownerAccountID
}

// instanceMatchesFilters checks whether a VM + its built ec2.Instance copy satisfy all parsed filters.
func instanceMatchesFilters(inst *vm.VM, ic *ec2.Instance, filters map[string][]string) bool {
	for name, values := range filters {
		if strings.HasPrefix(name, "tag:") {
			continue
		}

		var field string
		switch name {
		case "instance-state-name":
			if ic.State != nil && ic.State.Name != nil {
				field = *ic.State.Name
			}
		case "instance-id":
			field = inst.ID
		case "instance-type":
			field = inst.InstanceType
		case "vpc-id":
			if ic.VpcId != nil {
				field = *ic.VpcId
			}
		case "subnet-id":
			if ic.SubnetId != nil {
				field = *ic.SubnetId
			}
		case "tag-key":
			if !matchTagKey(ic.Tags, values) {
				return false
			}
			continue
		case "tag-value":
			if !matchTagValue(ic.Tags, values) {
				return false
			}
			continue
		default:
			return false
		}

		if !filterutil.MatchesAny(values, field) {
			return false
		}
	}

	tags := filterutil.EC2TagsToMap(ic.Tags)
	return filterutil.MatchesTags(filters, tags)
}

func matchTagKey(tags []*ec2.Tag, values []string) bool {
	for _, t := range tags {
		if t.Key != nil && filterutil.MatchesAny(values, *t.Key) {
			return true
		}
	}
	return false
}

func matchTagValue(tags []*ec2.Tag, values []string) bool {
	for _, t := range tags {
		if t.Value != nil && filterutil.MatchesAny(values, *t.Value) {
			return true
		}
	}
	return false
}

// DescribeInstances returns instances on this node visible to the caller's account.
func (s *InstanceServiceImpl) DescribeInstances(ctx context.Context, input *ec2.DescribeInstancesInput, accountID string) (*ec2.DescribeInstancesOutput, error) {
	slog.InfoContext(ctx, "Processing DescribeInstances request from this node", "accountID", accountID)

	instanceIDFilter := make(map[string]bool)
	for _, id := range input.InstanceIds {
		if id != nil && *id != "" {
			if !strings.HasPrefix(*id, "i-") {
				return nil, errors.New(awserrors.ErrorInvalidInstanceIDMalformed)
			}
			instanceIDFilter[*id] = true
		}
	}

	parsedFilters, err := filterutil.ParseFilters(input.Filters, DescribeInstancesValidFilters)
	if err != nil {
		slog.WarnContext(ctx, "DescribeInstances: invalid filter", "err", err)
		return nil, errors.New(awserrors.ErrorInvalidParameterValue)
	}

	reservationMap := make(map[string]*ec2.Reservation)

	// Iterate under the manager lock to avoid races on Status, Instance, PublicIP, etc.
	s.vmMgr.View(func(vms map[string]*vm.VM) {
		for _, instance := range vms {
			if !IsInstanceVisible(accountID, instance.AccountID) {
				continue
			}
			// Platform-managed VMs (LB, EKS control plane) are system-account-owned
			// and hidden from customer listings; only the system/Global account
			// sees them. The EKS reconciler resolves its control-plane VM's state
			// by running describe as the system account.
			if instance.ManagedBy != "" && accountID != utils.GlobalAccountID {
				continue
			}
			if len(instanceIDFilter) > 0 && !instanceIDFilter[instance.ID] {
				continue
			}
			if instance.Reservation == nil || instance.Instance == nil {
				continue
			}

			resID := ""
			if instance.Reservation.ReservationId != nil {
				resID = *instance.Reservation.ReservationId
			}

			if _, exists := reservationMap[resID]; !exists {
				reservation := &ec2.Reservation{}
				reservation.SetReservationId(resID)
				if instance.Reservation.OwnerId != nil {
					reservation.SetOwnerId(*instance.Reservation.OwnerId)
				}
				reservation.Instances = []*ec2.Instance{}
				reservationMap[resID] = reservation
			}

			instanceCopy := *instance.Instance
			instanceCopy.State = &ec2.InstanceState{}

			if instance.PublicIP != "" && instanceCopy.PublicIpAddress == nil {
				instanceCopy.PublicIpAddress = aws.String(instance.PublicIP)
			}

			if info, ok := vm.EC2APIState(instance.Status); ok {
				instanceCopy.State.SetCode(info.Code)
				instanceCopy.State.SetName(info.Name)
			} else {
				slog.WarnContext(ctx, "Instance has unmapped status, reporting as pending",
					"instanceId", instance.ID, "status", string(instance.Status))
				instanceCopy.State.SetCode(0)
				instanceCopy.State.SetName("pending")
			}

			if instance.PlacementGroupName != "" {
				instanceCopy.Placement = &ec2.Placement{
					GroupName:        aws.String(instance.PlacementGroupName),
					AvailabilityZone: aws.String(s.config.AZ),
				}
			}

			if len(parsedFilters) > 0 && !instanceMatchesFilters(instance, &instanceCopy, parsedFilters) {
				continue
			}

			reservationMap[resID].Instances = append(reservationMap[resID].Instances, &instanceCopy)
		}
	})

	reservations := make([]*ec2.Reservation, 0, len(reservationMap))
	for _, reservation := range reservationMap {
		reservations = append(reservations, reservation)
	}

	slog.InfoContext(ctx, "DescribeInstances completed", "count", len(reservations))
	return &ec2.DescribeInstancesOutput{Reservations: reservations}, nil
}

// DescribeInstanceTypes returns supported instance types. With filter
// capacity=true, expands each type to one entry per free slot for cluster-wide
// capacity aggregation.
func (s *InstanceServiceImpl) DescribeInstanceTypes(ctx context.Context, input *ec2.DescribeInstanceTypesInput, _ string) (*ec2.DescribeInstanceTypesOutput, error) {
	slog.InfoContext(ctx, "Processing DescribeInstanceTypes request from this node")

	if s.resourceMgr == nil {
		return nil, errors.New(awserrors.ErrorServerInternal)
	}

	showCapacity := false
	for _, f := range input.Filters {
		if f.Name != nil && *f.Name == "capacity" {
			for _, v := range f.Values {
				if v != nil && *v == "true" {
					showCapacity = true
					break
				}
			}
		}
	}

	var filteredTypes []*ec2.InstanceTypeInfo
	if showCapacity {
		filteredTypes = s.resourceMgr.GetAvailableInstanceTypeInfos(true)
	} else {
		filteredTypes = s.resourceMgr.GetSupportedInstanceTypeInfos()
	}
	slog.InfoContext(ctx, "DescribeInstanceTypes completed", "count", len(filteredTypes), "showCapacity", showCapacity)
	return &ec2.DescribeInstanceTypesOutput{InstanceTypes: filteredTypes}, nil
}

// DescribeStoppedInstances returns stopped instances from shared KV.
func (s *InstanceServiceImpl) DescribeStoppedInstances(ctx context.Context, input *ec2.DescribeInstancesInput, accountID string) (*ec2.DescribeInstancesOutput, error) {
	if s.stoppedStore == nil {
		return nil, errors.New(awserrors.ErrorServerInternal)
	}
	return s.describeInstancesFromKV(ctx, input, accountID, s.stoppedStore.ListStoppedInstances, 80, "stopped", "DescribeStoppedInstances")
}

// DescribeTerminatedInstances returns terminated instances from the terminated KV bucket.
func (s *InstanceServiceImpl) DescribeTerminatedInstances(ctx context.Context, input *ec2.DescribeInstancesInput, accountID string) (*ec2.DescribeInstancesOutput, error) {
	if s.stoppedStore == nil {
		return nil, errors.New(awserrors.ErrorServerInternal)
	}
	return s.describeInstancesFromKV(ctx, input, accountID, s.stoppedStore.ListTerminatedInstances, 48, "terminated", "DescribeTerminatedInstances")
}

func (s *InstanceServiceImpl) describeInstancesFromKV(ctx context.Context, input *ec2.DescribeInstancesInput, accountID string, listFn func() ([]*vm.VM, error), fallbackCode int64, fallbackName, opName string) (*ec2.DescribeInstancesOutput, error) {
	instanceIDFilter := make(map[string]bool)
	for _, id := range input.InstanceIds {
		if id != nil {
			instanceIDFilter[*id] = true
		}
	}

	parsedFilters, filterErr := filterutil.ParseFilters(input.Filters, DescribeInstancesValidFilters)
	if filterErr != nil {
		slog.WarnContext(ctx, opName+": invalid filter", "err", filterErr)
		return nil, errors.New(awserrors.ErrorInvalidParameterValue)
	}

	instances, err := listFn()
	if err != nil {
		slog.ErrorContext(ctx, opName+": failed to list instances", "err", err)
		return nil, errors.New(awserrors.ErrorServerInternal)
	}

	reservationMap := make(map[string]*ec2.Reservation)

	for _, instance := range instances {
		if !IsInstanceVisible(accountID, instance.AccountID) {
			continue
		}
		if len(instanceIDFilter) > 0 && !instanceIDFilter[instance.ID] {
			continue
		}
		if instance.Reservation == nil || instance.Instance == nil {
			slog.WarnContext(ctx, opName+": skipping instance with nil Reservation/Instance (data integrity issue)",
				"instanceId", instance.ID)
			continue
		}

		resID := ""
		if instance.Reservation.ReservationId != nil {
			resID = *instance.Reservation.ReservationId
		}

		if _, exists := reservationMap[resID]; !exists {
			reservation := &ec2.Reservation{}
			reservation.SetReservationId(resID)
			if instance.Reservation.OwnerId != nil {
				reservation.SetOwnerId(*instance.Reservation.OwnerId)
			}
			reservation.Instances = []*ec2.Instance{}
			reservationMap[resID] = reservation
		}

		instanceCopy := *instance.Instance
		instanceCopy.State = &ec2.InstanceState{}
		if info, ok := vm.EC2APIState(instance.Status); ok {
			instanceCopy.State.SetCode(info.Code)
			instanceCopy.State.SetName(info.Name)
		} else {
			instanceCopy.State.SetCode(fallbackCode)
			instanceCopy.State.SetName(fallbackName)
		}

		if len(parsedFilters) > 0 && !instanceMatchesFilters(instance, &instanceCopy, parsedFilters) {
			continue
		}

		reservationMap[resID].Instances = append(reservationMap[resID].Instances, &instanceCopy)
	}

	reservations := make([]*ec2.Reservation, 0, len(reservationMap))
	for _, reservation := range reservationMap {
		reservations = append(reservations, reservation)
	}

	slog.InfoContext(ctx, opName+" completed", "count", len(reservations))
	return &ec2.DescribeInstancesOutput{Reservations: reservations}, nil
}

// ModifyInstanceAttribute applies a single attribute change. SourceDestCheck=true
// is a no-op; false is unsupported because OVN port security enforces the check.
// InstanceType/UserData require a stopped instance.
func (s *InstanceServiceImpl) ModifyInstanceAttribute(ctx context.Context, input *ec2.ModifyInstanceAttributeInput, accountID string) (*ec2.ModifyInstanceAttributeOutput, error) {
	if input.InstanceId == nil || *input.InstanceId == "" {
		return nil, errors.New(awserrors.ErrorMissingParameter)
	}
	instanceID := *input.InstanceId

	if input.SourceDestCheck != nil {
		if input.SourceDestCheck.Value != nil && !*input.SourceDestCheck.Value {
			return nil, errors.New(awserrors.ErrorUnsupported)
		}
		slog.InfoContext(ctx, "ModifyInstanceAttribute: accepting SourceDestCheck=true (no-op)", "instanceId", instanceID)
		return &ec2.ModifyInstanceAttributeOutput{}, nil
	}

	// DisableApiTermination applies to both running and stopped instances.
	// Try the running map first; fall through to the stopped-store path only
	// if the instance isn't currently running on this node.
	if input.DisableApiTermination != nil && input.DisableApiTermination.Value != nil {
		newVal := input.DisableApiTermination.Value
		var notVisible bool
		updated, persistErr := s.vmMgr.UpdateAndPersist(instanceID, func(v *vm.VM) bool {
			if !IsInstanceVisible(accountID, v.AccountID) {
				notVisible = true
				return false
			}
			if v.RunInstancesInput == nil {
				v.RunInstancesInput = &ec2.RunInstancesInput{}
			}
			v.RunInstancesInput.DisableApiTermination = newVal
			return true
		})
		if notVisible {
			slog.WarnContext(ctx, "ModifyInstanceAttribute: instance not visible",
				"instanceId", instanceID, "callerAccount", accountID)
			return nil, errors.New(awserrors.ErrorInvalidInstanceIDNotFound)
		}
		if updated {
			if persistErr != nil {
				slog.ErrorContext(ctx, "ModifyInstanceAttribute: persist failed",
					"instanceId", instanceID, "err", persistErr)
				return nil, errors.New(awserrors.ErrorServerInternal)
			}
			slog.InfoContext(ctx, "ModifyInstanceAttribute: updated DisableApiTermination on running instance",
				"instanceId", instanceID, "value", *newVal)
			return &ec2.ModifyInstanceAttributeOutput{}, nil
		}
		// Not in running map — fall through to stopped-store path.
	}

	if s.stoppedStore == nil {
		slog.ErrorContext(ctx, "ModifyInstanceAttribute: stopped store not available")
		return nil, errors.New(awserrors.ErrorServerInternal)
	}

	instance, err := s.stoppedStore.LoadStoppedInstance(instanceID)
	if err != nil {
		slog.ErrorContext(ctx, "ModifyInstanceAttribute: failed to load stopped instance", "instanceId", instanceID, "err", err)
		return nil, errors.New(awserrors.ErrorServerInternal)
	}
	if instance == nil {
		slog.WarnContext(ctx, "ModifyInstanceAttribute: instance not found in shared KV", "instanceId", instanceID)
		return nil, errors.New(awserrors.ErrorInvalidInstanceIDNotFound)
	}

	if instance.Status != vm.StateStopped {
		slog.ErrorContext(ctx, "ModifyInstanceAttribute: instance not in stopped state", "instanceId", instanceID, "status", instance.Status)
		return nil, errors.New(awserrors.ErrorIncorrectInstanceState)
	}

	if !IsInstanceVisible(accountID, instance.AccountID) {
		slog.WarnContext(ctx, "ModifyInstanceAttribute: instance not visible",
			"instanceId", instanceID, "callerAccount", accountID, "ownerAccount", instance.AccountID)
		return nil, errors.New(awserrors.ErrorInvalidInstanceIDNotFound)
	}

	if input.InstanceType != nil && input.InstanceType.Value != nil {
		newType := *input.InstanceType.Value
		if newType == "" {
			slog.ErrorContext(ctx, "ModifyInstanceAttribute: empty instance type value", "instanceId", instanceID)
			return nil, errors.New(awserrors.ErrorInvalidInstanceAttributeValue)
		}
		if instance.Instance == nil {
			slog.ErrorContext(ctx, "ModifyInstanceAttribute: instance.Instance is nil, data integrity issue", "instanceId", instanceID)
			return nil, errors.New(awserrors.ErrorServerInternal)
		}
		slog.InfoContext(ctx, "ModifyInstanceAttribute: changing instance type",
			"instanceId", instanceID, "oldType", instance.InstanceType, "newType", newType)
	}

	// CAS the mutations into the shared KV record instead of a wholesale
	// WriteStoppedInstance: createIfAbsent=false means a claim that deletes
	// the record between the Load above and this write fails cleanly with
	// nats.ErrKeyNotFound rather than resurrecting a stale stopped entry.
	_, err = s.stoppedStore.UpdateStoppedInstance(instanceID, func(v *vm.VM) {
		if input.InstanceType != nil && input.InstanceType.Value != nil && v.Instance != nil {
			newType := *input.InstanceType.Value
			v.InstanceType = newType
			v.Config.InstanceType = newType
			v.Instance.InstanceType = aws.String(newType)
			// Clear StateReason — resolves capacity-unavailable state from instance-type-missing bug.
			v.Instance.StateReason = nil
		}

		if input.UserData != nil && input.UserData.Value != nil {
			slog.InfoContext(ctx, "ModifyInstanceAttribute: changing user data", "instanceId", instanceID)
			if v.RunInstancesInput == nil {
				v.RunInstancesInput = &ec2.RunInstancesInput{}
			}
			v.RunInstancesInput.UserData = aws.String(base64.StdEncoding.EncodeToString(input.UserData.Value))
		}

		if input.DisableApiTermination != nil && input.DisableApiTermination.Value != nil {
			slog.InfoContext(ctx, "ModifyInstanceAttribute: changing DisableApiTermination on stopped instance",
				"instanceId", instanceID, "value", *input.DisableApiTermination.Value)
			if v.RunInstancesInput == nil {
				v.RunInstancesInput = &ec2.RunInstancesInput{}
			}
			v.RunInstancesInput.DisableApiTermination = input.DisableApiTermination.Value
		}
	})
	if err != nil {
		if errors.Is(err, nats.ErrKeyNotFound) {
			// The record existed moments ago (Load above) but a concurrent
			// claim removed it: the instance is mid-transition, not gone.
			slog.WarnContext(ctx, "ModifyInstanceAttribute: instance claimed concurrently, not resurrecting", "instanceId", instanceID)
			return nil, errors.New(awserrors.ErrorIncorrectInstanceState)
		}
		slog.ErrorContext(ctx, "ModifyInstanceAttribute: failed to write modified instance to KV",
			"instanceId", instanceID, "err", err)
		return nil, errors.New(awserrors.ErrorServerInternal)
	}

	slog.InfoContext(ctx, "ModifyInstanceAttribute: completed successfully", "instanceId", instanceID)
	return &ec2.ModifyInstanceAttributeOutput{}, nil
}

// ModifyInstanceMetadataOptions applies a metadata-options change; only the hop
// limit is mutable. It mirrors DisableApiTermination's running-first/stopped path
// — no stopped-only guard, since AWS permits this on running instances.
func (s *InstanceServiceImpl) ModifyInstanceMetadataOptions(ctx context.Context, input *ec2.ModifyInstanceMetadataOptionsInput, accountID string) (*ec2.ModifyInstanceMetadataOptionsOutput, error) {
	if input.InstanceId == nil || *input.InstanceId == "" {
		return nil, errors.New(awserrors.ErrorMissingParameter)
	}
	instanceID := *input.InstanceId

	if err := validateMetadataOptions(
		aws.StringValue(input.HttpTokens),
		aws.StringValue(input.HttpEndpoint),
		aws.StringValue(input.HttpProtocolIpv6),
		aws.StringValue(input.InstanceMetadataTags),
		input.HttpPutResponseHopLimit,
	); err != nil {
		return nil, err
	}

	// Running-first: mutate the live instance's block, falling through to the
	// stopped store only when the instance isn't running on this node.
	var notVisible, integrityErr bool
	var options *ec2.InstanceMetadataOptionsResponse
	updated, persistErr := s.vmMgr.UpdateAndPersist(instanceID, func(v *vm.VM) bool {
		if !IsInstanceVisible(accountID, v.AccountID) {
			notVisible = true
			return false
		}
		if v.Instance == nil {
			integrityErr = true
			return false
		}
		applyMetadataOptions(v.Instance, input.HttpPutResponseHopLimit)
		opt := *v.Instance.MetadataOptions
		options = &opt
		return true
	})
	if notVisible {
		slog.WarnContext(ctx, "ModifyInstanceMetadataOptions: instance not visible",
			"instanceId", instanceID, "callerAccount", accountID)
		return nil, errors.New(awserrors.ErrorInvalidInstanceIDNotFound)
	}
	if integrityErr {
		slog.ErrorContext(ctx, "ModifyInstanceMetadataOptions: instance.Instance is nil, data integrity issue", "instanceId", instanceID)
		return nil, errors.New(awserrors.ErrorServerInternal)
	}
	if updated {
		if persistErr != nil {
			slog.ErrorContext(ctx, "ModifyInstanceMetadataOptions: persist failed", "instanceId", instanceID, "err", persistErr)
			return nil, errors.New(awserrors.ErrorServerInternal)
		}
		slog.InfoContext(ctx, "ModifyInstanceMetadataOptions: updated running instance", "instanceId", instanceID)
		return &ec2.ModifyInstanceMetadataOptionsOutput{InstanceId: aws.String(instanceID), InstanceMetadataOptions: options}, nil
	}

	if s.stoppedStore == nil {
		slog.ErrorContext(ctx, "ModifyInstanceMetadataOptions: stopped store not available")
		return nil, errors.New(awserrors.ErrorServerInternal)
	}

	instance, err := s.stoppedStore.LoadStoppedInstance(instanceID)
	if err != nil {
		slog.ErrorContext(ctx, "ModifyInstanceMetadataOptions: failed to load stopped instance", "instanceId", instanceID, "err", err)
		return nil, errors.New(awserrors.ErrorServerInternal)
	}
	if instance == nil {
		slog.WarnContext(ctx, "ModifyInstanceMetadataOptions: instance not found", "instanceId", instanceID)
		return nil, errors.New(awserrors.ErrorInvalidInstanceIDNotFound)
	}
	if !IsInstanceVisible(accountID, instance.AccountID) {
		slog.WarnContext(ctx, "ModifyInstanceMetadataOptions: instance not visible",
			"instanceId", instanceID, "callerAccount", accountID, "ownerAccount", instance.AccountID)
		return nil, errors.New(awserrors.ErrorInvalidInstanceIDNotFound)
	}
	if instance.Instance == nil {
		slog.ErrorContext(ctx, "ModifyInstanceMetadataOptions: instance.Instance is nil, data integrity issue", "instanceId", instanceID)
		return nil, errors.New(awserrors.ErrorServerInternal)
	}

	applyMetadataOptions(instance.Instance, input.HttpPutResponseHopLimit)
	if err := s.stoppedStore.WriteStoppedInstance(instanceID, instance); err != nil {
		slog.ErrorContext(ctx, "ModifyInstanceMetadataOptions: failed to persist stopped instance", "instanceId", instanceID, "err", err)
		return nil, errors.New(awserrors.ErrorServerInternal)
	}
	slog.InfoContext(ctx, "ModifyInstanceMetadataOptions: updated stopped instance", "instanceId", instanceID)
	return &ec2.ModifyInstanceMetadataOptionsOutput{InstanceId: aws.String(instanceID), InstanceMetadataOptions: instance.Instance.MetadataOptions}, nil
}

// StoppedInstanceNode returns the node that last hosted the stopped instance,
// used to route start requests back to the original node when possible.
func (s *InstanceServiceImpl) StoppedInstanceNode(instanceID string) string {
	if s.stoppedStore == nil {
		return ""
	}
	instance, err := s.stoppedStore.LoadStoppedInstance(instanceID)
	if err != nil || instance == nil {
		return ""
	}
	return instance.LastNode
}

// StartStoppedInstance picks up a stopped instance from shared KV, re-launches
// it on this daemon node, then removes it from shared KV.
func (s *InstanceServiceImpl) StartStoppedInstance(ctx context.Context, input *StartStoppedInstanceInput, accountID string) (*StartStoppedInstanceOutput, error) {
	if input.InstanceID == "" {
		return nil, errors.New(awserrors.ErrorMissingParameter)
	}
	if s.stoppedStore == nil {
		slog.ErrorContext(ctx, "StartStoppedInstance: stopped store not available")
		return nil, errors.New(awserrors.ErrorServerInternal)
	}
	if s.resourceMgr == nil {
		slog.ErrorContext(ctx, "StartStoppedInstance: resource manager not available")
		return nil, errors.New(awserrors.ErrorServerInternal)
	}
	if s.vmMgr == nil {
		slog.ErrorContext(ctx, "StartStoppedInstance: vm manager not available")
		return nil, errors.New(awserrors.ErrorServerInternal)
	}

	instance, err := s.stoppedStore.LoadStoppedInstance(input.InstanceID)
	if err != nil {
		slog.ErrorContext(ctx, "StartStoppedInstance: failed to load stopped instance", "instanceId", input.InstanceID, "err", err)
		return nil, errors.New(awserrors.ErrorServerInternal)
	}
	if instance == nil {
		// Disambiguate a genuinely-absent record from a local race: if this
		// node's vmMgr already shows the instance running, a winning claim
		// started it here between another caller's checks and this Load, so
		// the correct taxonomy is IncorrectInstanceState, not NotFound. A
		// cross-node winner is invisible to this node's vmMgr and still
		// falls through to NotFound.
		if _, ok := s.vmMgr.Get(input.InstanceID); ok {
			slog.WarnContext(ctx, "StartStoppedInstance: instance already running locally", "instanceId", input.InstanceID)
			return nil, errors.New(awserrors.ErrorIncorrectInstanceState)
		}
		slog.WarnContext(ctx, "StartStoppedInstance: instance not found in shared KV", "instanceId", input.InstanceID)
		return nil, errors.New(awserrors.ErrorInvalidInstanceIDNotFound)
	}
	if instance.Status != vm.StateStopped {
		slog.ErrorContext(ctx, "StartStoppedInstance: instance not in stopped state", "instanceId", input.InstanceID, "status", instance.Status)
		return nil, errors.New(awserrors.ErrorIncorrectInstanceState)
	}
	if !IsInstanceVisible(accountID, instance.AccountID) {
		slog.WarnContext(ctx, "StartStoppedInstance: instance not visible",
			"instanceId", input.InstanceID, "callerAccount", accountID, "ownerAccount", instance.AccountID)
		return nil, errors.New(awserrors.ErrorInvalidInstanceIDNotFound)
	}

	// Atomic claim: at most one caller across the cluster can win this race
	// — a forwarded call and this node's local fallback, or two nodes
	// racing the same forwarded call, both pass the checks above, but only
	// one succeeds here. The loser must not touch resourceMgr/vmMgr at
	// all, closing the double-start-onto-the-same-volume race. This also
	// replaces the plain Load used above as the record of truth going
	// forward: `instance` is reassigned to the freshest claimed value.
	instance, err = s.stoppedStore.ClaimStoppedInstance(input.InstanceID)
	if err != nil {
		if errors.Is(err, vm.ErrStoppedInstanceClaimed) {
			slog.WarnContext(ctx, "StartStoppedInstance: lost race to claim stopped instance",
				"instanceId", input.InstanceID)
			return nil, errors.New(awserrors.ErrorIncorrectInstanceState)
		}
		slog.ErrorContext(ctx, "StartStoppedInstance: failed to claim stopped instance",
			"instanceId", input.InstanceID, "err", err)
		return nil, errors.New(awserrors.ErrorServerInternal)
	}

	// Reset node-local fields that are stale after cross-node migration.
	instance.ResetNodeLocalState()

	instanceType, ok := s.resourceMgr.InstanceTypes()[instance.InstanceType]
	if !ok {
		slog.ErrorContext(ctx, "StartStoppedInstance: instance type not available on this node",
			"instanceId", input.InstanceID, "instanceType", instance.InstanceType)
		s.restoreClaimedStoppedInstance(ctx, instance)
		return nil, errors.New(awserrors.ErrorInsufficientInstanceCapacity)
	}
	if err := s.resourceMgr.Allocate(instanceType); err != nil {
		slog.ErrorContext(ctx, "StartStoppedInstance: failed to allocate resources", "instanceId", input.InstanceID, "err", err)
		s.restoreClaimedStoppedInstance(ctx, instance)
		return nil, errors.New(awserrors.ErrorInsufficientInstanceCapacity)
	}

	// Add to local map + clear stop attribute before launch.
	instance.Attributes = spxtypes.EC2CommandAttributes{StartInstance: true}
	s.vmMgr.Insert(instance)

	// Claim GPU for GPU instance types.
	gpuClaimed := false
	if s.gpuClaimer != nil && instancetypes.IsGPUType(instanceType) {
		profileName := instancetypes.MIGProfileFromType(aws.StringValue(instanceType.InstanceType))
		att, gpuErr := s.gpuClaimer.Claim(instance.ID, profileName)
		if gpuErr != nil {
			slog.ErrorContext(ctx, "StartStoppedInstance: GPU claim failed", "instanceId", input.InstanceID, "err", gpuErr)
			s.resourceMgr.Deallocate(instanceType)
			s.vmMgr.Delete(instance.ID)
			s.restoreClaimedStoppedInstance(ctx, instance)
			return nil, errors.New(awserrors.ErrorInsufficientInstanceCapacity)
		}
		instance.GPUAttachments = []gpu.GPUAttachment{*att}
		gpuClaimed = true
		slog.InfoContext(ctx, "GPU claimed for instance", "instanceId", input.InstanceID,
			"pci", att.PCIAddress, "mdev", att.MdevPath)
	}

	if err := s.vmMgr.Run(ctx, instance); err != nil {
		slog.ErrorContext(ctx, "StartStoppedInstance: vmMgr.Run failed", "instanceId", input.InstanceID, "err", err)
		if gpuClaimed {
			if relErr := s.gpuClaimer.Release(instance.ID); relErr != nil {
				slog.ErrorContext(ctx, "StartStoppedInstance: GPU release failed after launch failure",
					"instanceId", input.InstanceID, "err", relErr)
			}
		}
		s.resourceMgr.Deallocate(instanceType)
		s.vmMgr.Delete(instance.ID)
		s.restoreClaimedStoppedInstance(ctx, instance)
		return nil, errors.New(awserrors.ErrorServerInternal)
	}

	// Discover actual guest device names via QMP query-block.
	s.vmMgr.UpdateGuestDeviceNames(instance)

	slog.InfoContext(ctx, "Started stopped instance from shared KV", "instanceId", instance.ID)
	return &StartStoppedInstanceOutput{Status: "running", InstanceID: instance.ID}, nil
}

// restoreClaimedStoppedInstance writes instance back to the shared
// stopped-instance store after a downstream failure that follows a
// successful ClaimStoppedInstance (which already removed it), so the claim
// does not silently lose the instance. Best-effort: the caller already has
// an error to return, so a restore failure is logged, not surfaced — the
// instance is still safely stopped in the local vm.Manager's view (it was
// never inserted, or was removed again above).
func (s *InstanceServiceImpl) restoreClaimedStoppedInstance(ctx context.Context, instance *vm.VM) {
	if err := s.stoppedStore.WriteStoppedInstance(instance.ID, instance); err != nil {
		slog.ErrorContext(ctx, "StartStoppedInstance: failed to restore claimed instance to shared KV after downstream failure",
			"instanceId", instance.ID, "err", err)
	}
}

// TerminateStoppedInstance terminates a stopped instance: deletes its volumes,
// releases its public IP and ENI, and moves it to the terminated KV bucket.
func (s *InstanceServiceImpl) TerminateStoppedInstance(ctx context.Context, input *TerminateStoppedInstanceInput, accountID string) (*TerminateStoppedInstanceOutput, error) {
	if input.InstanceID == "" {
		return nil, errors.New(awserrors.ErrorMissingParameter)
	}
	if s.stoppedStore == nil {
		slog.ErrorContext(ctx, "TerminateStoppedInstance: stopped store not available")
		return nil, errors.New(awserrors.ErrorServerInternal)
	}

	instance, err := s.stoppedStore.LoadStoppedInstance(input.InstanceID)
	if err != nil {
		slog.ErrorContext(ctx, "TerminateStoppedInstance: failed to load stopped instance", "instanceId", input.InstanceID, "err", err)
		return nil, errors.New(awserrors.ErrorServerInternal)
	}
	if instance == nil {
		slog.WarnContext(ctx, "TerminateStoppedInstance: instance not found in shared KV", "instanceId", input.InstanceID)
		return nil, errors.New(awserrors.ErrorInvalidInstanceIDNotFound)
	}
	if instance.Status != vm.StateStopped {
		slog.ErrorContext(ctx, "TerminateStoppedInstance: instance not in stopped state", "instanceId", input.InstanceID, "status", instance.Status)
		return nil, errors.New(awserrors.ErrorIncorrectInstanceState)
	}
	if !IsInstanceVisible(accountID, instance.AccountID) {
		slog.WarnContext(ctx, "TerminateStoppedInstance: instance not visible",
			"instanceId", input.InstanceID, "callerAccount", accountID, "ownerAccount", instance.AccountID)
		return nil, errors.New(awserrors.ErrorInvalidInstanceIDNotFound)
	}

	if instance.IsTerminationProtected() {
		slog.WarnContext(ctx, "TerminateStoppedInstance: instance has termination protection",
			"instanceId", input.InstanceID)
		return nil, errors.New(awserrors.ErrorOperationNotPermitted)
	}

	s.deleteInstanceVolumes(ctx, instance, input.InstanceID)
	s.releaseInstancePublicIP(ctx, instance, input.InstanceID)
	s.deleteInstanceENI(ctx, instance, input.InstanceID)

	// Write to terminated KV FIRST so the instance is visible in DescribeInstances.
	// If this fails the instance stays in the stopped bucket — safe to retry.
	instance.Status = vm.StateTerminated
	if err := s.stoppedStore.WriteTerminatedInstance(input.InstanceID, instance); err != nil {
		slog.ErrorContext(ctx, "TerminateStoppedInstance: failed to write to terminated KV, aborting", "instanceId", input.InstanceID, "err", err)
		return nil, errors.New(awserrors.ErrorServerInternal)
	}

	// Now safe to remove from stopped KV. Retry once on failure so the instance
	// doesn't appear in both buckets.
	if err := s.stoppedStore.DeleteStoppedInstance(input.InstanceID); err != nil {
		slog.WarnContext(ctx, "TerminateStoppedInstance: first stopped KV delete failed, retrying",
			"instanceId", input.InstanceID, "err", err)
		if retryErr := s.stoppedStore.DeleteStoppedInstance(input.InstanceID); retryErr != nil {
			slog.ErrorContext(ctx, "TerminateStoppedInstance: stopped KV delete failed after retry, instance may appear in both buckets",
				"instanceId", input.InstanceID, "err", retryErr)
		}
	}

	s.deleteCentralInstanceTags(ctx, input.InstanceID, instance.AccountID)

	slog.InfoContext(ctx, "Terminated stopped instance from shared KV", "instanceId", input.InstanceID)
	return &TerminateStoppedInstanceOutput{Status: "terminated", InstanceID: input.InstanceID}, nil
}

func (s *InstanceServiceImpl) deleteInstanceVolumes(ctx context.Context, instance *vm.VM, instanceID string) {
	instance.EBSRequests.Mu.Lock()
	defer instance.EBSRequests.Mu.Unlock()
	for _, ebsRequest := range instance.EBSRequests.Requests {
		// Internal volumes (EFI) are always cleaned up via ebs.delete.
		if ebsRequest.EFI {
			ebsDeleteData, err := json.Marshal(spxtypes.EBSDeleteRequest{Volume: ebsRequest.Name})
			if err != nil {
				slog.ErrorContext(ctx, "TerminateStoppedInstance: failed to marshal ebs.delete request", "name", ebsRequest.Name, "err", err)
				continue
			}
			deleteMsg, err := s.natsConn.Request("ebs.delete", ebsDeleteData, 30*time.Second)
			if err != nil {
				slog.WarnContext(ctx, "TerminateStoppedInstance: ebs.delete failed for internal volume", "name", ebsRequest.Name, "err", err)
			} else {
				slog.InfoContext(ctx, "TerminateStoppedInstance: ebs.delete sent for internal volume", "name", ebsRequest.Name, "data", string(deleteMsg.Data))
			}
			continue
		}

		// User-visible volumes: respect DeleteOnTermination.
		if !ebsRequest.DeleteOnTermination {
			slog.InfoContext(ctx, "TerminateStoppedInstance: volume has DeleteOnTermination=false, skipping", "name", ebsRequest.Name)
			continue
		}

		slog.InfoContext(ctx, "TerminateStoppedInstance: deleting volume with DeleteOnTermination=true", "name", ebsRequest.Name)
		if s.volumeDeleter == nil {
			slog.WarnContext(ctx, "TerminateStoppedInstance: volume deleter not configured, skipping", "name", ebsRequest.Name)
			continue
		}
		if _, err := s.volumeDeleter.DeleteVolume(ctx, &ec2.DeleteVolumeInput{
			VolumeId: &ebsRequest.Name,
		}, instance.AccountID); err != nil {
			slog.ErrorContext(ctx, "TerminateStoppedInstance: failed to delete volume", "name", ebsRequest.Name, "err", err)
		}
	}
	_ = instanceID
}

// rollbackAutoAssignedPublicIP unwinds a failed auto-assign: clears the ENI
// public IP record, releases the IPAM lease, then detaches and deletes the ENI.
func (s *InstanceServiceImpl) rollbackAutoAssignedPublicIP(ctx context.Context, accountID, instanceID, eniID, publicIP, poolName string) {
	if s.eniCreator != nil {
		if err := s.eniCreator.UpdateENIPublicIP(ctx, accountID, eniID, "", ""); err != nil {
			slog.WarnContext(ctx, "PrepareRunInstances: failed to clear ENI public IP during NAT-failure rollback",
				"eniId", eniID, "publicIp", publicIP, "err", err)
		}
	}
	if s.ipReleaser != nil {
		if err := s.ipReleaser.ReleaseIP(ctx, poolName, publicIP, eniID); err != nil {
			slog.WarnContext(ctx, "PrepareRunInstances: failed to release public IP during NAT-failure rollback",
				"publicIp", publicIP, "pool", poolName, "err", err)
		}
	}
	if s.eniCreator != nil {
		if err := s.eniCreator.DetachENI(ctx, accountID, eniID); err != nil {
			slog.WarnContext(ctx, "PrepareRunInstances: failed to detach ENI during NAT-failure rollback",
				"eniId", eniID, "instanceId", instanceID, "err", err)
		}
	}
	if s.eniDeleter != nil {
		if _, err := s.eniDeleter.DeleteNetworkInterface(ctx, &ec2.DeleteNetworkInterfaceInput{
			NetworkInterfaceId: &eniID,
		}, accountID); err != nil {
			slog.WarnContext(ctx, "PrepareRunInstances: failed to delete ENI during NAT-failure rollback",
				"eniId", eniID, "instanceId", instanceID, "err", err)
		}
	}
}

func (s *InstanceServiceImpl) releaseInstancePublicIP(ctx context.Context, instance *vm.VM, instanceID string) {
	if instance.PublicIP == "" || instance.PublicIPPool == "" || s.ipReleaser == nil {
		return
	}
	portName := topology.Port(instance.ENIId)
	vpcID := ""
	logicalIP := ""
	if instance.Instance != nil {
		if instance.Instance.VpcId != nil {
			vpcID = *instance.Instance.VpcId
		}
		if instance.Instance.PrivateIpAddress != nil {
			logicalIP = *instance.Instance.PrivateIpAddress
		}
	}
	utils.PublishNATEvent(s.natsConn, "vpc.delete-nat", vpcID, instance.PublicIP, logicalIP, portName, "")

	if err := s.ipReleaser.ReleaseIP(ctx, instance.PublicIPPool, instance.PublicIP, instance.ENIId); err != nil {
		slog.WarnContext(ctx, "TerminateStoppedInstance: failed to release public IP", "ip", instance.PublicIP, "pool", instance.PublicIPPool, "err", err)
	} else {
		slog.InfoContext(ctx, "TerminateStoppedInstance: released public IP", "ip", instance.PublicIP, "instanceId", instanceID)
	}
}

func (s *InstanceServiceImpl) deleteInstanceENI(ctx context.Context, instance *vm.VM, instanceID string) {
	if instance.ENIId == "" || s.eniDeleter == nil {
		return
	}
	// Detach first to clear the attachment record. A stopped instance's ENI may
	// still show Status=in-use/AttachmentId set; without the detach the
	// force=false DeleteNetworkInterface below fails InvalidNetworkInterface.InUse,
	// stranding the ENI record after the instance is already gone from the store
	// and blocking VPC/subnet delete. Best-effort: the delete is
	// the authoritative gate.
	if s.eniCreator != nil {
		if err := s.eniCreator.DetachENI(ctx, instance.AccountID, instance.ENIId); err != nil {
			slog.WarnContext(ctx, "TerminateStoppedInstance: failed to detach ENI before delete",
				"eni", instance.ENIId, "instanceId", instanceID, "err", err)
		}
	}
	_, err := s.eniDeleter.DeleteNetworkInterface(ctx, &ec2.DeleteNetworkInterfaceInput{
		NetworkInterfaceId: &instance.ENIId,
	}, instance.AccountID)
	switch {
	case err == nil,
		awserrors.IsErrorCode(err, awserrors.ErrorInvalidNetworkInterfaceIDNotFound),
		awserrors.IsErrorCode(err, awserrors.ErrorInvalidNetworkInterfaceNotFound):
		slog.InfoContext(ctx, "TerminateStoppedInstance: deleted ENI", "eni", instance.ENIId, "instanceId", instanceID)
	default:
		slog.ErrorContext(ctx, "TerminateStoppedInstance: failed to delete ENI", "eni", instance.ENIId, "err", err)
	}
}

// DescribeInstanceAttribute returns a single requested attribute for an instance.
// Checks running instances first (in-memory), then falls back to stopped instances in KV.
func (s *InstanceServiceImpl) DescribeInstanceAttribute(ctx context.Context, input *ec2.DescribeInstanceAttributeInput, accountID string) (*ec2.DescribeInstanceAttributeOutput, error) {
	if input.InstanceId == nil || *input.InstanceId == "" {
		return nil, errors.New(awserrors.ErrorMissingParameter)
	}
	if input.Attribute == nil || *input.Attribute == "" {
		return nil, errors.New(awserrors.ErrorMissingParameter)
	}

	instanceID := *input.InstanceId
	attribute := *input.Attribute

	var instance *vm.VM
	if running, ok := s.vmMgr.Get(instanceID); ok {
		instance = running
	}

	if instance == nil {
		if s.stoppedStore == nil {
			slog.ErrorContext(ctx, "DescribeInstanceAttribute: stopped store not available")
			return nil, errors.New(awserrors.ErrorServerInternal)
		}
		stopped, err := s.stoppedStore.LoadStoppedInstance(instanceID)
		if err != nil {
			slog.ErrorContext(ctx, "DescribeInstanceAttribute: failed to load stopped instance",
				"instanceId", instanceID, "err", err)
			return nil, errors.New(awserrors.ErrorServerInternal)
		}
		instance = stopped
	}

	if instance == nil {
		slog.WarnContext(ctx, "DescribeInstanceAttribute: instance not found",
			"instanceId", instanceID)
		return nil, errors.New(awserrors.ErrorInvalidInstanceIDNotFound)
	}

	if !IsInstanceVisible(accountID, instance.AccountID) {
		slog.WarnContext(ctx, "DescribeInstanceAttribute: instance not visible",
			"instanceId", instanceID, "callerAccount", accountID, "ownerAccount", instance.AccountID)
		return nil, errors.New(awserrors.ErrorInvalidInstanceIDNotFound)
	}

	output := &ec2.DescribeInstanceAttributeOutput{
		InstanceId: &instanceID,
	}

	switch attribute {
	case ec2.InstanceAttributeNameInstanceType:
		val := instance.InstanceType
		output.InstanceType = &ec2.AttributeValue{Value: &val}

	case ec2.InstanceAttributeNameUserData:
		// AWS returns user-data base64-encoded; RunInstancesInput.UserData is the
		// canonical base64 store, set at launch and kept in sync by Modify.
		var val string
		if instance.RunInstancesInput != nil && instance.RunInstancesInput.UserData != nil {
			val = *instance.RunInstancesInput.UserData
		}
		output.UserData = &ec2.AttributeValue{Value: &val}

	case ec2.InstanceAttributeNameDisableApiTermination:
		// Read under the manager lock so we serialise with a concurrent
		// ModifyInstanceAttribute writer. Inspect is a no-op race-wise for
		// stopped instances (no concurrent writers) but keeps the call site
		// uniform.
		var val bool
		s.vmMgr.Inspect(instance, func(v *vm.VM) {
			val = v.IsTerminationProtected()
		})
		output.DisableApiTermination = &ec2.AttributeBooleanValue{Value: &val}

	case ec2.InstanceAttributeNameDisableApiStop:
		val := false
		output.DisableApiStop = &ec2.AttributeBooleanValue{Value: &val}

	case ec2.InstanceAttributeNameInstanceInitiatedShutdownBehavior:
		val := ec2.ShutdownBehaviorStop
		output.InstanceInitiatedShutdownBehavior = &ec2.AttributeValue{Value: &val}

	case ec2.InstanceAttributeNameEbsOptimized:
		val := false
		output.EbsOptimized = &ec2.AttributeBooleanValue{Value: &val}

	case ec2.InstanceAttributeNameEnaSupport:
		val := true
		output.EnaSupport = &ec2.AttributeBooleanValue{Value: &val}

	case ec2.InstanceAttributeNameSourceDestCheck:
		val := true
		output.SourceDestCheck = &ec2.AttributeBooleanValue{Value: &val}

	case ec2.InstanceAttributeNameGroupSet:
		if instance.Instance != nil && len(instance.Instance.SecurityGroups) > 0 {
			output.Groups = instance.Instance.SecurityGroups
		} else {
			output.Groups = []*ec2.GroupIdentifier{}
		}

	default:
		slog.WarnContext(ctx, "DescribeInstanceAttribute: unsupported attribute",
			"instanceId", instanceID, "attribute", attribute)
		return nil, errors.New(awserrors.ErrorInvalidParameterValue)
	}

	slog.InfoContext(ctx, "DescribeInstanceAttribute: completed",
		"instanceId", instanceID, "attribute", attribute, "accountID", accountID)
	return output, nil
}

// Terminated and error states are deliberately never surfaced: terminated
// matches AWS (drops from DescribeInstanceStatus shortly after termination);
// error is a Spinifex-internal state whose name is not a valid AWS state label.
var (
	describeInstanceStatusRunningOnly = map[vm.InstanceState]bool{vm.StateRunning: true}
	describeInstanceStatusAllIncluded = map[vm.InstanceState]bool{
		vm.StateRunning:      true,
		vm.StatePending:      true,
		vm.StateProvisioning: true,
		vm.StateStopping:     true,
		vm.StateStopped:      true,
		vm.StateShuttingDown: true,
	}
)

// hostHealthReporter is an optional capability on the resource manager that
// reports node-level memory pressure, surfaced as DescribeInstanceStatus
// SystemStatus. Implemented by daemon.ResourceManager.
type hostHealthReporter interface {
	HostUnderMemoryPressure() bool
}

// hostUnderMemoryPressure reports whether the node is under memory pressure when
// the resource manager supports the check, false otherwise (fail open).
func (s *InstanceServiceImpl) hostUnderMemoryPressure() bool {
	if hr, ok := s.resourceMgr.(hostHealthReporter); ok {
		return hr.HostUnderMemoryPressure()
	}
	return false
}

func (s *InstanceServiceImpl) buildInstanceStatus(v *vm.VM, systemImpaired bool) *ec2.InstanceStatus {
	state := &ec2.InstanceState{}
	if info, ok := vm.EC2StateCodes[v.Status]; ok {
		state.SetCode(info.Code)
		state.SetName(info.Name)
	} else {
		state.SetCode(vm.EC2StateCodes[vm.StatePending].Code)
		state.SetName(vm.EC2StateCodes[vm.StatePending].Name)
	}

	status, reachability, impairedSince := instanceHealthSummary(v)

	instDetail := &ec2.InstanceStatusDetails{
		Name:   aws.String(reachabilityDetailName),
		Status: aws.String(reachability),
	}
	if impairedSince != nil {
		instDetail.ImpairedSince = impairedSince
	}

	// SystemStatus reflects host/node health, independent of the VM process: a
	// running VM's host is reachable unless under memory pressure; non-running
	// instances are not-applicable.
	systemStatus, systemReach := instanceStatusOK, instanceStatusPassed
	switch {
	case v.Status != vm.StateRunning:
		systemStatus, systemReach = instanceStatusNotApplicable, instanceStatusNotApplicable
	case systemImpaired:
		systemStatus, systemReach = instanceStatusImpaired, instanceStatusFailed
	}

	return &ec2.InstanceStatus{
		AvailabilityZone: aws.String(s.config.AZ),
		InstanceId:       aws.String(v.ID),
		InstanceState:    state,
		InstanceStatus: &ec2.InstanceStatusSummary{
			Status:  aws.String(status),
			Details: []*ec2.InstanceStatusDetails{instDetail},
		},
		SystemStatus: &ec2.InstanceStatusSummary{
			Status: aws.String(systemStatus),
			Details: []*ec2.InstanceStatusDetails{{
				Name:   aws.String(reachabilityDetailName),
				Status: aws.String(systemReach),
			}},
		},
	}
}

// instanceInitializingGrace is the post-launch window during which a running VM
// reports initializing, matching AWS's status-check grace period.
const instanceInitializingGrace = 2 * time.Minute

// instanceHealthSummary maps a VM's runtime and QMP health into AWS
// InstanceStatus fields: the status label, the reachability detail status, and
// an optional ImpairedSince timestamp. Only running VMs carry real health; all
// other states are not-applicable per AWS behavior.
func instanceHealthSummary(v *vm.VM) (status, reachability string, impairedSince *time.Time) {
	if v.Status != vm.StateRunning {
		return instanceStatusNotApplicable, instanceStatusNotApplicable, nil
	}

	// QMP unresponsive past the failure gate → impaired/failed.
	if v.Health.QMPConsecutiveFailures >= vm.QMPMaxConsecutiveFailures {
		var impPtr *time.Time
		if !v.Health.ImpairedSince.IsZero() {
			since := v.Health.ImpairedSince
			impPtr = &since
		}
		return instanceStatusImpaired, instanceStatusFailed, impPtr
	}

	// Grace period: freshly launched VMs report initializing until reachable.
	if v.Instance != nil && v.Instance.LaunchTime != nil &&
		time.Since(*v.Instance.LaunchTime) < instanceInitializingGrace {
		return instanceStatusInitializing, instanceStatusInitializing, nil
	}

	return instanceStatusOK, instanceStatusPassed, nil
}

const (
	instanceStatusOK            = "ok"
	instanceStatusPassed        = "passed"
	instanceStatusNotApplicable = "not-applicable"
	instanceStatusImpaired      = "impaired"
	instanceStatusFailed        = "failed"
	instanceStatusInitializing  = "initializing"
	reachabilityDetailName      = "reachability"
)

func instanceStatusMatchesFilters(v *vm.VM, is *ec2.InstanceStatus, filters map[string][]string) bool {
	for name, values := range filters {
		if strings.HasPrefix(name, "tag:") {
			continue
		}
		var field string
		switch name {
		case "availability-zone":
			if is.AvailabilityZone != nil {
				field = *is.AvailabilityZone
			}
		case "instance-state-name":
			if is.InstanceState != nil && is.InstanceState.Name != nil {
				field = *is.InstanceState.Name
			}
		case "instance-state-code":
			if is.InstanceState != nil && is.InstanceState.Code != nil {
				field = strconv.FormatInt(*is.InstanceState.Code, 10)
			}
		default:
			return false
		}
		if !filterutil.MatchesAny(values, field) {
			return false
		}
	}

	if v.Instance != nil {
		tags := filterutil.EC2TagsToMap(v.Instance.Tags)
		return filterutil.MatchesTags(filters, tags)
	}
	return filterutil.MatchesTags(filters, nil)
}

// DescribeInstanceStatus returns per-VM status entries for VMs on this node
// visible to the caller. Stopped instances come from the gateway's KV query,
// not this handler.
func (s *InstanceServiceImpl) DescribeInstanceStatus(ctx context.Context, input *ec2.DescribeInstanceStatusInput, accountID string) (*ec2.DescribeInstanceStatusOutput, error) {
	slog.InfoContext(ctx, "Processing DescribeInstanceStatus request from this node", "accountID", accountID)

	instanceIDFilter := make(map[string]bool)
	for _, id := range input.InstanceIds {
		if id == nil || *id == "" {
			continue
		}
		if !strings.HasPrefix(*id, "i-") {
			return nil, errors.New(awserrors.ErrorInvalidInstanceIDMalformed)
		}
		instanceIDFilter[*id] = true
	}

	parsedFilters, err := filterutil.ParseFilters(input.Filters, DescribeInstanceStatusValidFilters)
	if err != nil {
		slog.WarnContext(ctx, "DescribeInstanceStatus: invalid filter", "err", err)
		return nil, errors.New(awserrors.ErrorInvalidParameterValue)
	}

	includedStates := describeInstanceStatusRunningOnly
	if aws.BoolValue(input.IncludeAllInstances) {
		includedStates = describeInstanceStatusAllIncluded
	}

	// SystemStatus is node-wide: evaluate host memory pressure once per request
	// rather than per instance.
	systemImpaired := s.hostUnderMemoryPressure()

	var statuses []*ec2.InstanceStatus
	s.vmMgr.View(func(vms map[string]*vm.VM) {
		for _, v := range vms {
			if !IsInstanceVisible(accountID, v.AccountID) {
				continue
			}
			if len(instanceIDFilter) > 0 && !instanceIDFilter[v.ID] {
				continue
			}
			if !includedStates[v.Status] {
				continue
			}
			is := s.buildInstanceStatus(v, systemImpaired)
			if len(parsedFilters) > 0 && !instanceStatusMatchesFilters(v, is, parsedFilters) {
				continue
			}
			statuses = append(statuses, is)
		}
	})

	slog.InfoContext(ctx, "DescribeInstanceStatus completed", "count", len(statuses))
	return &ec2.DescribeInstanceStatusOutput{InstanceStatuses: statuses}, nil
}
