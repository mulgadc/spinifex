package daemon

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
	"github.com/mulgadc/spinifex/spinifex/vm"
	"github.com/nats-io/nats.go"
)

// startStoppedForwardTimeout bounds the wait for ec2.start.{nodeId} to respond.
// StartStoppedInstance does volume rehydrate + QEMU launch + QMP handshake,
// which can take 10-20s on a cold start. Match the awsgw upstream 30s budget
// so a slow-but-alive target node never trips the fallback path — falling back
// while the target is still launching causes a duplicate Run that collides on
// the deterministic tap name (TUNSETIFF: Device or resource busy).
const startStoppedForwardTimeout = 30 * time.Second

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

	// Targeted launch: the gateway routes only when an explicit reservation id is
	// present, but the id rides in the input either way. Validate semantics
	// up-front (the per-instance atomic re-check under rm.mu covers the race);
	// the count gate in PrepareRunInstances handles a full reservation.
	reservationID := capacityReservationTargetID(input)
	if reservationID != "" {
		if it, ok := d.resourceMgr.instanceTypes[aws.StringValue(input.InstanceType)]; ok {
			if vErr := d.resourceMgr.ValidateReservationTarget(reservationID, accountID, it); vErr != nil {
				respondWithError(msg, awserrors.ValidErrorCode(vErr.Error()))
				return
			}
		}
	}

	reservation, instances, instanceType, err := d.instanceService.PrepareRunInstances(input, accountID, reservationID)
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
			if reservationID == "" {
				d.resourceMgr.deallocate(instanceType)
			} else {
				d.resourceMgr.ReleaseToReservation(reservationID, instanceType)
			}
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

// capacityReservationTargetID returns the explicit targeted-launch reservation id
// from the input, or "" when the launch is untargeted (general path).
func capacityReservationTargetID(input *ec2.RunInstancesInput) string {
	spec := input.CapacityReservationSpecification
	if spec == nil || spec.CapacityReservationTarget == nil {
		return ""
	}
	return aws.StringValue(spec.CapacityReservationTarget.CapacityReservationId)
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

				// Map internal status to AWS state, projecting Spinifex-only states
				// (e.g. error -> stopped) so SDK/UI clients see a valid label.
				if info, ok := vm.EC2APIState(instance.Status); ok {
					instanceCopy.State.SetCode(info.Code)
					instanceCopy.State.SetName(info.Name)
				} else {
					slog.Warn("Instance has unmapped status, reporting as pending",
						"instanceId", instance.ID, "status", string(instance.Status))
					instanceCopy.State.SetCode(0)
					instanceCopy.State.SetName("pending")
				}

				// Project IamInstanceProfile from vm.VM (single source of truth
				// across Associate/Disassociate/Replace lifecycle). Id is left
				// nil — the gateway resolves it via IAMService post-aggregation
				// since daemons have no IAM access. Empty Arn clears any stale
				// reference left on stored instance.Instance (e.g. after
				// Disassociate or auto-clear on terminate).
				if instance.IamInstanceProfileArn != "" {
					instanceCopy.IamInstanceProfile = &ec2.IamInstanceProfile{
						Arn: aws.String(instance.IamInstanceProfileArn),
					}
				} else {
					instanceCopy.IamInstanceProfile = nil
				}

				// Populate Placement if instance belongs to a placement group
				if instance.PlacementGroupName != "" {
					instanceCopy.Placement = &ec2.Placement{
						GroupName:        aws.String(instance.PlacementGroupName),
						AvailabilityZone: aws.String(d.config.AZ),
					}
				}

				// Echo the consumed capacity reservation so targeted-launch
				// Terraform converges — without it the instance reports no
				// reservation and the plan never settles.
				if instance.CapacityReservationId != "" {
					instanceCopy.CapacityReservationId = aws.String(instance.CapacityReservationId)
					instanceCopy.CapacityReservationSpecification = &ec2.CapacityReservationSpecificationResponse{
						CapacityReservationPreference: aws.String(ec2.CapacityReservationPreferenceOpen),
						CapacityReservationTarget: &ec2.CapacityReservationTargetResponse{
							CapacityReservationId: aws.String(instance.CapacityReservationId),
						},
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

// handleEC2DescribeInstanceStatus responds with status entries for VMs on this node visible to the caller.
func (d *Daemon) handleEC2DescribeInstanceStatus(msg *nats.Msg) {
	handleNATSRequest(msg, d.instanceService.DescribeInstanceStatus)
}

// handleEC2StartStoppedInstance handles the generic ec2.start queue-group topic.
// It reads the stopped instance's LastNode from shared KV and forwards the
// request to ec2.start.{lastNode} when the instance last ran on a different
// node. This keeps instances on their original node so the per-node resource
// manager sees the correct allocation. Local fallback fires only when the
// targeted node has no active subscriber (ErrNoResponders — node down or not
// yet recovered) or returns InsufficientInstanceCapacity. A timeout from a
// reachable-but-slow target is surfaced as ServerInternal so the caller can
// retry; falling back in that case races the still-running launch on the
// target and collides on the deterministic tap name.
func (d *Daemon) handleEC2StartStoppedInstance(msg *nats.Msg) {
	// Peek at the instance ID without full unmarshal — we only need it for the
	// LastNode lookup. The full unmarshal happens inside StartStoppedInstance.
	var peek struct {
		InstanceID string `json:"instance_id"`
	}
	if err := json.Unmarshal(msg.Data, &peek); err != nil || peek.InstanceID == "" {
		// Can't determine target node — fall through to local start which will
		// return the appropriate error (missing parameter / unmarshal failure).
		handleNATSRequest(msg, d.instanceService.StartStoppedInstance)
		return
	}

	lastNode := d.instanceService.StoppedInstanceNode(peek.InstanceID)
	if lastNode != "" && lastNode != d.node {
		targetTopic := fmt.Sprintf("ec2.start.%s", lastNode)
		forwardMsg := nats.NewMsg(targetTopic)
		forwardMsg.Data = msg.Data
		forwardMsg.Header.Set(utils.AccountIDHeader, utils.AccountIDFromMsg(msg))

		slog.Info("ec2.start: forwarding to original node",
			"instanceId", peek.InstanceID, "lastNode", lastNode)
		resp, err := d.natsConn.RequestMsg(forwardMsg, startStoppedForwardTimeout)
		if err == nil {
			// ValidateErrorPayload returns a non-nil error when the payload IS an
			// AWS error response; nil means it is a success payload.
			errPayload, isErrPayload := utils.ValidateErrorPayload(resp.Data)
			isCapacity := isErrPayload != nil &&
				errPayload.Code != nil &&
				*errPayload.Code == awserrors.ErrorInsufficientInstanceCapacity

			if !isCapacity {
				// Relay success or any non-capacity error as-is.
				if relayErr := msg.Respond(resp.Data); relayErr != nil {
					slog.Error("ec2.start: failed to relay response from original node",
						"instanceId", peek.InstanceID, "lastNode", lastNode, "err", relayErr)
				}
				return
			}
			slog.Warn("ec2.start: original node at capacity, starting locally",
				"instanceId", peek.InstanceID, "lastNode", lastNode)
		} else if errors.Is(err, nats.ErrNoResponders) {
			// Target node has no active subscription — down or restarting. Fall
			// back to local start so the instance can resume somewhere.
			slog.Warn("ec2.start: original node has no subscriber, starting locally",
				"instanceId", peek.InstanceID, "lastNode", lastNode, "err", err)
		} else {
			// Timeout or other transport error from a node whose subscription
			// did exist. Do NOT fall back — the target may still be launching
			// the VM, and a duplicate Run here would collide on tap setup.
			slog.Error("ec2.start: forward to original node failed, not falling back",
				"instanceId", peek.InstanceID, "lastNode", lastNode, "err", err)
			respondWithError(msg, awserrors.ErrorServerInternal)
			return
		}
	}

	handleNATSRequest(msg, d.instanceService.StartStoppedInstance)
}

// handleEC2StartStoppedInstanceDirect handles ec2.start.{nodeId} — the
// node-targeted topic used by handleEC2StartStoppedInstance to forward start
// requests back to the original node. Always starts locally; never forwards
// further, preventing routing loops.
func (d *Daemon) handleEC2StartStoppedInstanceDirect(msg *nats.Msg) {
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
		if info, ok := vm.EC2APIState(instance.Status); ok {
			instanceCopy.State.SetCode(info.Code)
			instanceCopy.State.SetName(info.Name)
		} else {
			instanceCopy.State.SetCode(fallbackCode)
			instanceCopy.State.SetName(fallbackName)
		}

		// Project IamInstanceProfile from vm.VM (cleared on terminate;
		// preserved across stop/start). Mirrors handleEC2DescribeInstances.
		if instance.IamInstanceProfileArn != "" {
			instanceCopy.IamInstanceProfile = &ec2.IamInstanceProfile{
				Arn: aws.String(instance.IamInstanceProfileArn),
			}
		} else {
			instanceCopy.IamInstanceProfile = nil
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

func (d *Daemon) handleEC2ModifyInstanceMetadataOptions(msg *nats.Msg) {
	handleNATSRequest(msg, d.instanceService.ModifyInstanceMetadataOptions)
}

// handleEC2DescribeInstanceAttribute returns a single requested attribute for an instance.
func (d *Daemon) handleEC2DescribeInstanceAttribute(msg *nats.Msg) {
	handleNATSRequest(msg, d.instanceService.DescribeInstanceAttribute)
}
