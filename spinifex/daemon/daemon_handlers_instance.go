package daemon

import (
	"context"
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
	handlers_ec2_instance "github.com/mulgadc/spinifex/spinifex/handlers/ec2/instance"
	"github.com/mulgadc/spinifex/spinifex/types"
	"github.com/mulgadc/spinifex/spinifex/utils"
	"github.com/mulgadc/spinifex/spinifex/vm"
	"github.com/nats-io/nats.go"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
)

// startStoppedForwardTimeout bounds the wait for ec2.start.{nodeId} to respond.
// StartStoppedInstance does volume rehydrate + QEMU launch + QMP handshake,
// which can take 10-20s on a cold start. Match the awsgw upstream 30s budget.
// A var (not const) so tests can shrink it instead of sleeping real seconds.
var startStoppedForwardTimeout = 30 * time.Second

// handleSetInstanceTags applies a create-tags/delete-tags mutation to a running
// instance: central store first, then the record under the manager lock, so a
// failed S3 write leaves both stores untouched, matching the stopped path.
// Ownership is checked by checkInstanceOwnership before dispatch.
func (d *Daemon) handleSetInstanceTags(ctx context.Context, msg *nats.Msg, command types.EC2InstanceCommand, instance *vm.VM) {
	remove := command.Attributes.RemoveInstanceTags
	data := command.InstanceTagsData
	if data == nil || (!remove && len(data.Tags) == 0) {
		respondWithError(msg, awserrors.ErrorMissingParameter)
		return
	}

	var newTags []*ec2.Tag
	missingRecord := false
	d.vmMgr.Inspect(instance, func(v *vm.VM) {
		if v.Instance == nil {
			missingRecord = true
			return
		}
		newTags = handlers_ec2_instance.ApplyInstanceTagMutation(v.Instance.Tags, data, remove)
	})
	if missingRecord {
		respondWithError(msg, awserrors.ErrorServerInternal)
		return
	}

	accountID := utils.AccountIDFromMsg(msg)
	if err := d.tagsService.PutResourceTags(ctx, accountID, instance.ID, handlers_ec2_instance.TagsToMap(newTags)); err != nil {
		slog.ErrorContext(ctx, "SetInstanceTags: central tag store write failed",
			"instanceId", instance.ID, "err", err)
		respondWithError(msg, awserrors.ErrorServerInternal)
		return
	}

	found, err := d.vmMgr.UpdateAndPersist(instance.ID, func(v *vm.VM) bool {
		if v.Instance == nil {
			return false
		}
		v.Instance.Tags = handlers_ec2_instance.ApplyInstanceTagMutation(v.Instance.Tags, data, remove)
		return true
	})
	if err != nil {
		respondWithError(msg, awserrors.ValidErrorCodeFromError(err))
		return
	}
	if !found {
		respondWithError(msg, awserrors.ErrorInvalidInstanceIDNotFound)
		return
	}

	if err := msg.Respond([]byte(`{}`)); err != nil {
		slog.ErrorContext(ctx, "Failed to respond to NATS request", "err", err)
	}
}

// handleEC2RunInstances orchestrates the RunInstances flow across
// InstanceService.PrepareRunInstances (validation + ENI creation),
// daemon-local Insert/WriteState/per-instance subscribe, and
// InstanceService.LaunchRunInstances (volumes + GPU + vmMgr.Run). The split
// preserves the original respond-then-launch timing — AWS gets a reservation
// before the launch loop starts.
func (d *Daemon) handleEC2RunInstances(msg *nats.Msg) {
	ctx, span := utils.StartConsumerSpan(msg)
	defer span.End()
	slog.DebugContext(ctx, "Received message on subject", "subject", msg.Subject)

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
				respondWithError(msg, awserrors.ValidErrorCodeFromError(vErr))
				return
			}
		}
	}

	_, prepSpan := otel.Tracer(daemonTracerName).Start(ctx, "ec2.PrepareRunInstances")
	reservation, instances, instanceType, err := d.instanceService.PrepareRunInstances(ctx, input, accountID, reservationID)
	endOpSpan(prepSpan, err)
	if err != nil {
		respondWithError(msg, awserrors.ValidErrorCodeFromError(err))
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

	// Project launch tags into the central tag store so describe-tags agrees
	// with the record from birth. Best-effort: the reservation is already
	// returned, so a failed write is logged rather than failing the launch.
	for _, instance := range instances {
		if instance.Instance == nil || len(instance.Instance.Tags) == 0 {
			continue
		}
		if err := d.tagsService.PutResourceTags(ctx, accountID, instance.ID,
			handlers_ec2_instance.TagsToMap(instance.Instance.Tags)); err != nil {
			slog.Error("handleEC2RunInstances: launch tag central store write failed",
				"instanceId", instance.ID, "err", err)
		}
	}

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

	_, launchSpan := otel.Tracer(daemonTracerName).Start(ctx, "ec2.LaunchRunInstances",
		trace.WithAttributes(attribute.Int("instance.count", len(instances))))
	d.instanceService.LaunchRunInstances(ctx, instances, input, instanceType)
	launchSpan.End()
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

	// Validate the requested instance IDs, rejecting malformed ones so this path
	// and the stopped/terminated path below enforce the same rule.
	instanceIDFilter, err := handlers_ec2_instance.ParseInstanceIDFilter(describeInstancesInput.InstanceIds)
	if err != nil {
		respondWithError(msg, awserrors.ErrorInvalidInstanceIDMalformed)
		return
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

				// Project every vm.VM-sourced field onto an API-shaped copy via the
				// shared projection, so this path and the stopped/terminated path
				// below define the field set exactly once and cannot drift. Running
				// instances carry their runtime network (public IP, DNS) and
				// capacity reservation; an unmapped status falls back to pending/0.
				projected, stateMapped := handlers_ec2_instance.ProjectInstance(instance, handlers_ec2_instance.InstanceProjection{
					Region:                d.config.Region,
					DNSBaseDomain:         d.dnsBaseDomain,
					DNSInternalDomain:     d.dnsInternalDomain,
					AZ:                    d.config.AZ,
					IncludeRuntimeNetwork: true,
					FallbackStateCode:     0,
					FallbackStateName:     "pending",
				})
				if !stateMapped {
					slog.Warn("Instance has unmapped status, reporting as pending",
						"instanceId", instance.ID, "status", string(instance.Status))
				}

				// Apply filters against the fully-built instance copy
				if len(parsedFilters) > 0 && !instanceMatchesFilters(instance, projected, parsedFilters) {
					continue
				}

				// Add instance to its reservation
				reservationMap[resID].Instances = append(reservationMap[resID].Instances, projected)
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

// handleEC2StartStoppedInstance handles the generic ec2.start queue-group topic.
// It reads the stopped instance's LastNode from shared KV and forwards the
// request to ec2.start.{lastNode} to keep instances on their original node.
// Local fallback fires on any forward failure — no responders, capacity, or
// timeout — because StartStoppedInstance's ClaimStoppedInstance call is the
// single cluster-wide serialization point, so a losing racer bails out
// cleanly instead of double-starting.
func (d *Daemon) handleEC2StartStoppedInstance(msg *nats.Msg) {
	// Peek at the instance ID without full unmarshal — we only need it for the
	// LastNode lookup. The full unmarshal happens inside StartStoppedInstance.
	var peek struct {
		InstanceID string `json:"instance_id"`
	}
	if err := json.Unmarshal(msg.Data, &peek); err != nil || peek.InstanceID == "" {
		// Can't determine target node — fall through to local start which will
		// return the appropriate error (missing parameter / unmarshal failure).
		handleNATSRequest(d.instanceService.StartStoppedInstance)(msg)
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
			// Timeout or other transport error — e.g. lastNode crashed without
			// a clean NATS disconnect, so no ErrNoResponders fires. The atomic
			// claim in StartStoppedInstance makes a local retry safe either way.
			slog.Warn("ec2.start: forward to original node failed, attempting local start",
				"instanceId", peek.InstanceID, "lastNode", lastNode, "err", err)
		}
	}

	handleNATSRequest(d.instanceService.StartStoppedInstance)(msg)
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

	// Validate instance IDs identically to the running path — the gateway fans
	// out to both, so a malformed ID must fail the same way whichever responds.
	instanceIDFilter, err := handlers_ec2_instance.ParseInstanceIDFilter(describeInput.InstanceIds)
	if err != nil {
		respondWithError(msg, awserrors.ErrorInvalidInstanceIDMalformed)
		return
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

		// Reuse the shared projection so stopped/terminated instances carry the
		// same vm.VM-sourced fields as the running path — this is the fix for the
		// dropped Placement/Spot lineage. Runtime network (public IP, DNS) and the
		// capacity reservation are released on stop, so IncludeRuntimeNetwork stays
		// false and they remain absent. The caller's fallback labels the state when
		// a stored status has no EC2 mapping.
		projected, _ := handlers_ec2_instance.ProjectInstance(instance, handlers_ec2_instance.InstanceProjection{
			AZ:                d.config.AZ,
			FallbackStateCode: fallbackCode,
			FallbackStateName: fallbackName,
		})

		if len(parsedFilters) > 0 && !instanceMatchesFilters(instance, projected, parsedFilters) {
			continue
		}

		reservationMap[resID].Instances = append(reservationMap[resID].Instances, projected)
	}

	reservations := make([]*ec2.Reservation, 0, len(reservationMap))
	for _, reservation := range reservationMap {
		reservations = append(reservations, reservation)
	}

	respondWithJSON(msg, &ec2.DescribeInstancesOutput{Reservations: reservations})
	slog.Info(handlerName+" completed", "count", len(reservations))
}
