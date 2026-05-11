package daemon

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	"github.com/mulgadc/spinifex/spinifex/filterutil"
	"github.com/mulgadc/spinifex/spinifex/utils"
	"github.com/mulgadc/spinifex/spinifex/vm"
	"github.com/nats-io/nats.go"
)

// handleEC2RunInstances orchestrates the RunInstances flow across
// InstanceService.PrepareRunInstances (validation + ENI creation),
// daemon-local Insert/WriteState/per-instance subscribe, and
// InstanceService.LaunchRunInstances (volumes + GPU + vmMgr.Run). The split
// preserves the original respond-then-launch timing — AWS gets a reservation
// before the launch loop starts.
func (d *Daemon) handleEC2RunInstances(msg *nats.Msg) {
	slog.Debug("Received message on subject", "subject", msg.Subject)

	accountID := utils.AccountIDFromMsg(msg)
	if accountID == "" {
		slog.Error("handleEC2RunInstances: missing account ID in NATS header")
		respondWithError(msg, awserrors.ErrorServerInternal)
		return
	}

	input := &ec2.RunInstancesInput{}
	if errResp := utils.UnmarshalJsonPayload(input, msg.Data); errResp != nil {
		if err := msg.Respond(errResp); err != nil {
			slog.Error("Failed to respond to NATS request", "err", err)
		}
		slog.Error("Request does not match RunInstancesInput")
		return
	}

	reservation, instances, instanceType, err := d.instanceService.PrepareRunInstances(input, accountID)
	if err != nil {
		respondWithError(msg, awserrors.ValidErrorCode(err.Error()))
		return
	}

	// PlacementGroupNode is daemon-local identity, set after prepare.
	for _, instance := range instances {
		if instance.PlacementGroupName != "" {
			instance.PlacementGroupNode = d.node
		}
	}

	jsonResponse, err := json.Marshal(reservation)
	if err != nil {
		slog.Error("handleEC2RunInstances failed to marshal reservation", "err", err)
		respondWithError(msg, awserrors.ErrorServerInternal)
		for range instances {
			d.resourceMgr.deallocate(instanceType)
		}
		return
	}
	if err := msg.Respond(jsonResponse); err != nil {
		slog.Error("Failed to respond to NATS request", "err", err)
	}

	for _, instance := range instances {
		d.vmMgr.Insert(instance)
	}
	if err := d.WriteState(); err != nil {
		slog.Error("handleEC2RunInstances failed to write initial state", "err", err)
	}
	slog.Info("Instances added to state with pending status", "count", len(instances))

	// Subscribe per-instance NATS topics so terminate/stop reach this daemon
	// while volumes prepare. LaunchInstance replaces these on success.
	d.mu.Lock()
	for _, instance := range instances {
		sub, subErr := d.natsConn.Subscribe(fmt.Sprintf("ec2.cmd.%s", instance.ID), d.handleEC2Events)
		if subErr != nil {
			slog.Error("Failed to early-subscribe to per-instance topic", "instanceId", instance.ID, "err", subErr)
		} else {
			d.natsSubscriptions[instance.ID] = sub
		}
	}
	d.mu.Unlock()

	d.instanceService.LaunchRunInstances(instances, input, instanceType)
}

// describeInstancesValidFilters defines the set of filter names accepted by DescribeInstances.
var describeInstancesValidFilters = map[string]bool{
	"instance-state-name": true,
	"instance-id":         true,
	"instance-type":       true,
	"vpc-id":              true,
	"subnet-id":           true,
	"tag-key":             true,
	"tag-value":           true,
}

// instanceMatchesFilters checks whether a VM + its built ec2.Instance copy satisfy all parsed filters.
func instanceMatchesFilters(inst *vm.VM, ic *ec2.Instance, filters map[string][]string) bool {
	for name, values := range filters {
		if strings.HasPrefix(name, "tag:") {
			// tag:Key filters are handled after all field filters.
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
			// Filter name passed ParseFilters but has no case — treat as non-match
			// to avoid silently ignoring it.
			return false
		}

		if !filterutil.MatchesAny(values, field) {
			return false
		}
	}

	// Check tag:Key filters via the instance's Tag slice.
	tags := filterutil.EC2TagsToMap(ic.Tags)
	return filterutil.MatchesTags(filters, tags)
}

// matchTagKey returns true if any tag key on the resource matches any of the filter values.
func matchTagKey(tags []*ec2.Tag, values []string) bool {
	for _, t := range tags {
		if t.Key != nil && filterutil.MatchesAny(values, *t.Key) {
			return true
		}
	}
	return false
}

// matchTagValue returns true if any tag value on the resource matches any of the filter values.
func matchTagValue(tags []*ec2.Tag, values []string) bool {
	for _, t := range tags {
		if t.Value != nil && filterutil.MatchesAny(values, *t.Value) {
			return true
		}
	}
	return false
}

// handleEC2DescribeInstances responds with instances running on this node visible to the caller.
func (d *Daemon) handleEC2DescribeInstances(msg *nats.Msg) {
	slog.Debug("Received message", "subject", msg.Subject, "data", string(msg.Data))

	// Extract account ID from NATS header for scoping
	accountID := utils.AccountIDFromMsg(msg)

	// Initialize describeInstancesInput before unmarshaling into it
	describeInstancesInput := &ec2.DescribeInstancesInput{}
	errResp := utils.UnmarshalJsonPayload(describeInstancesInput, msg.Data)

	if errResp != nil {
		if err := msg.Respond(errResp); err != nil {
			slog.Error("Failed to respond to NATS request", "err", err)
		}
		slog.Error("Request does not match DescribeInstancesInput")
		return
	}

	slog.Info("Processing DescribeInstances request from this node", "accountID", accountID)

	// Validate and filter instances if specific instance IDs were requested
	instanceIDFilter := make(map[string]bool)
	if len(describeInstancesInput.InstanceIds) > 0 {
		for _, id := range describeInstancesInput.InstanceIds {
			if id != nil && *id != "" {
				if !strings.HasPrefix(*id, "i-") {
					respondWithError(msg, awserrors.ErrorInvalidInstanceIDMalformed)
					return
				}
				instanceIDFilter[*id] = true
			}
		}
	}

	// Parse filters (returns error for unknown filter names)
	parsedFilters, err := filterutil.ParseFilters(describeInstancesInput.Filters, describeInstancesValidFilters)
	if err != nil {
		slog.Warn("DescribeInstances: invalid filter", "err", err)
		respondWithError(msg, awserrors.ErrorInvalidParameterValue)
		return
	}

	// Group instances by reservation ID (AWS returns instances grouped by reservation)
	reservationMap := make(map[string]*ec2.Reservation)

	// Iterate under the manager lock — VM fields (Status, Instance, Reservation,
	// PublicIP, PlacementGroupName) are mutated through manager-locked
	// Inspect/UpdateState elsewhere, so reading them lock-free would race.
	d.vmMgr.View(func(vms map[string]*vm.VM) {
		for _, instance := range vms {
			// Skip instances not owned by the caller's account.
			// Instances with an empty AccountID (legacy/migration data)
			// are only visible to root.
			if !isInstanceVisible(accountID, instance.AccountID) {
				continue
			}

			// Skip if filtering by instance IDs and this instance is not in the filter
			if len(instanceIDFilter) > 0 && !instanceIDFilter[instance.ID] {
				continue
			}

			// Use stored reservation metadata if available
			if instance.Reservation != nil && instance.Instance != nil {
				resID := ""
				if instance.Reservation.ReservationId != nil {
					resID = *instance.Reservation.ReservationId
				}

				// Create reservation entry if it doesn't exist
				if _, exists := reservationMap[resID]; !exists {
					reservation := &ec2.Reservation{}
					reservation.SetReservationId(resID)
					if instance.Reservation.OwnerId != nil {
						reservation.SetOwnerId(*instance.Reservation.OwnerId)
					}
					reservation.Instances = []*ec2.Instance{}
					reservationMap[resID] = reservation
				}

				// Update the instance state to current state
				instanceCopy := *instance.Instance
				instanceCopy.State = &ec2.InstanceState{}

				// Populate PublicIpAddress from VM if stored
				if instance.PublicIP != "" && instanceCopy.PublicIpAddress == nil {
					instanceCopy.PublicIpAddress = aws.String(instance.PublicIP)
				}

				// Map internal status to EC2 state codes using the centralized mapping
				if info, ok := vm.EC2StateCodes[instance.Status]; ok {
					instanceCopy.State.SetCode(info.Code)
					instanceCopy.State.SetName(info.Name)
				} else {
					slog.Warn("Instance has unmapped status, reporting as pending",
						"instanceId", instance.ID, "status", string(instance.Status))
					instanceCopy.State.SetCode(0)
					instanceCopy.State.SetName("pending")
				}

				// Populate Placement if instance belongs to a placement group
				if instance.PlacementGroupName != "" {
					instanceCopy.Placement = &ec2.Placement{
						GroupName:        aws.String(instance.PlacementGroupName),
						AvailabilityZone: aws.String(d.config.AZ),
					}
				}

				// Apply filters against the fully-built instance copy
				if len(parsedFilters) > 0 && !instanceMatchesFilters(instance, &instanceCopy, parsedFilters) {
					continue
				}

				// Add instance to its reservation
				reservationMap[resID].Instances = append(reservationMap[resID].Instances, &instanceCopy)
			}
		}
	})

	// Convert map to slice
	reservations := make([]*ec2.Reservation, 0, len(reservationMap))
	for _, reservation := range reservationMap {
		reservations = append(reservations, reservation)
	}

	// Create the response
	output := &ec2.DescribeInstancesOutput{
		Reservations: reservations,
	}

	respondWithJSON(msg, output)
	slog.Info("handleEC2DescribeInstances completed", "count", len(reservations))
}

// handleEC2DescribeInstanceTypes responds with instance types provisionable on this node.
func (d *Daemon) handleEC2DescribeInstanceTypes(msg *nats.Msg) {
	handleNATSRequest(msg, d.instanceService.DescribeInstanceTypes)
}

// startStoppedInstanceRequest is the payload for ec2.start topic
func (d *Daemon) handleEC2StartStoppedInstance(msg *nats.Msg) {
	handleNATSRequest(msg, d.instanceService.StartStoppedInstance)
}

func (d *Daemon) handleEC2TerminateStoppedInstance(msg *nats.Msg) {
	handleNATSRequest(msg, d.instanceService.TerminateStoppedInstance)
}

// handleEC2DescribeStoppedInstances returns stopped instances from shared KV.
func (d *Daemon) handleEC2DescribeStoppedInstances(msg *nats.Msg) {
	if d.stateStore == nil {
		respondWithError(msg, awserrors.ErrorServerInternal)
		return
	}
	d.describeInstancesFromKV(msg, d.stateStore.ListStoppedInstances, 80, "stopped", "handleEC2DescribeStoppedInstances")
}

// handleEC2DescribeTerminatedInstances returns terminated instances from the terminated KV bucket.
func (d *Daemon) handleEC2DescribeTerminatedInstances(msg *nats.Msg) {
	if d.stateStore == nil {
		respondWithError(msg, awserrors.ErrorServerInternal)
		return
	}
	d.describeInstancesFromKV(msg, d.stateStore.ListTerminatedInstances, 48, "terminated", "handleEC2DescribeTerminatedInstances")
}

// describeInstancesFromKV is a shared helper for DescribeStopped/TerminatedInstances handlers.
// It lists instances from a KV bucket, filters by account/instance ID, and responds with reservations.
func (d *Daemon) describeInstancesFromKV(msg *nats.Msg, listFn func() ([]*vm.VM, error), fallbackCode int64, fallbackName, handlerName string) {
	accountID := utils.AccountIDFromMsg(msg)

	describeInput := &ec2.DescribeInstancesInput{}
	if len(msg.Data) > 0 {
		if errResp := utils.UnmarshalJsonPayload(describeInput, msg.Data); errResp != nil {
			if err := msg.Respond(errResp); err != nil {
				slog.Error("Failed to respond to NATS request", "err", err)
			}
			return
		}
	}

	instanceIDFilter := make(map[string]bool)
	for _, id := range describeInput.InstanceIds {
		if id != nil {
			instanceIDFilter[*id] = true
		}
	}

	parsedFilters, filterErr := filterutil.ParseFilters(describeInput.Filters, describeInstancesValidFilters)
	if filterErr != nil {
		slog.Warn(handlerName+": invalid filter", "err", filterErr)
		respondWithError(msg, awserrors.ErrorInvalidParameterValue)
		return
	}

	instances, err := listFn()
	if err != nil {
		slog.Error(handlerName+": failed to list instances", "err", err)
		respondWithError(msg, awserrors.ErrorServerInternal)
		return
	}

	reservationMap := make(map[string]*ec2.Reservation)

	for _, instance := range instances {
		if !isInstanceVisible(accountID, instance.AccountID) {
			continue
		}
		if len(instanceIDFilter) > 0 && !instanceIDFilter[instance.ID] {
			continue
		}
		if instance.Reservation == nil || instance.Instance == nil {
			slog.Warn(handlerName+": skipping instance with nil Reservation/Instance (data integrity issue)",
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
		if info, ok := vm.EC2StateCodes[instance.Status]; ok {
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

	respondWithJSON(msg, &ec2.DescribeInstancesOutput{Reservations: reservations})
	slog.Info(handlerName+" completed", "count", len(reservations))
}

func (d *Daemon) handleEC2ModifyInstanceAttribute(msg *nats.Msg) {
	handleNATSRequest(msg, d.instanceService.ModifyInstanceAttribute)
}

// handleEC2DescribeInstanceAttribute returns a single requested attribute for an instance.
func (d *Daemon) handleEC2DescribeInstanceAttribute(msg *nats.Msg) {
	var input ec2.DescribeInstanceAttributeInput
	if err := json.Unmarshal(msg.Data, &input); err != nil {
		slog.Error("handleEC2DescribeInstanceAttribute: failed to unmarshal request", "err", err)
		respondWithError(msg, awserrors.ErrorServerInternal)
		return
	}

	if input.InstanceId == nil || *input.InstanceId == "" {
		slog.Error("handleEC2DescribeInstanceAttribute: missing instance_id")
		respondWithError(msg, awserrors.ErrorMissingParameter)
		return
	}
	if input.Attribute == nil || *input.Attribute == "" {
		slog.Error("handleEC2DescribeInstanceAttribute: missing attribute")
		respondWithError(msg, awserrors.ErrorMissingParameter)
		return
	}

	instanceID := *input.InstanceId
	attribute := *input.Attribute
	accountID := utils.AccountIDFromMsg(msg)

	// Look up instance: running first, then stopped KV.
	var instance *vm.VM
	if running, ok := d.vmMgr.Get(instanceID); ok {
		instance = running
	}

	if instance == nil {
		if d.stateStore == nil {
			slog.Error("handleEC2DescribeInstanceAttribute: state store not available")
			respondWithError(msg, awserrors.ErrorServerInternal)
			return
		}
		stopped, err := d.stateStore.LoadStoppedInstance(instanceID)
		if err != nil {
			slog.Error("handleEC2DescribeInstanceAttribute: failed to load stopped instance",
				"instanceId", instanceID, "err", err)
			respondWithError(msg, awserrors.ErrorServerInternal)
			return
		}
		instance = stopped
	}

	if instance == nil {
		slog.Warn("handleEC2DescribeInstanceAttribute: instance not found",
			"instanceId", instanceID)
		respondWithError(msg, awserrors.ErrorInvalidInstanceIDNotFound)
		return
	}

	if !checkInstanceOwnership(msg, instanceID, instance.AccountID) {
		return
	}

	output := &ec2.DescribeInstanceAttributeOutput{
		InstanceId: &instanceID,
	}

	switch attribute {
	case ec2.InstanceAttributeNameInstanceType:
		val := instance.InstanceType
		output.InstanceType = &ec2.AttributeValue{Value: &val}

	case ec2.InstanceAttributeNameUserData:
		val := instance.UserData
		output.UserData = &ec2.AttributeValue{Value: &val}

	case ec2.InstanceAttributeNameDisableApiTermination:
		val := false
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
		slog.Warn("handleEC2DescribeInstanceAttribute: unsupported attribute",
			"instanceId", instanceID, "attribute", attribute)
		respondWithError(msg, awserrors.ErrorInvalidParameterValue)
		return
	}

	slog.Info("handleEC2DescribeInstanceAttribute: completed",
		"instanceId", instanceID, "attribute", attribute, "accountID", accountID)
	respondWithJSON(msg, output)
}
