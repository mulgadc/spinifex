package gateway_ec2_instance

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	handlers_iam "github.com/mulgadc/spinifex/spinifex/handlers/iam"
	"github.com/mulgadc/spinifex/spinifex/utils"
	"github.com/nats-io/nats.go"
)

// maxConcurrentAbsenceProofs bounds how many describes may hold a full fan-out
// deadline at once. Sized so the gateway can absorb a burst of genuine misses
// while the ceiling on pinned state stays flat.
const maxConcurrentAbsenceProofs = 64

// absenceProofSlots is the budget for describes that cannot settle early. A
// healthy cluster answers from every node and releases its slot in
// milliseconds; slots only pile up when a node has gone quiet, which is
// exactly when a caller could otherwise pin the gateway with free requests.
var absenceProofSlots = make(chan struct{}, maxConcurrentAbsenceProofs)

// acquireAbsenceProofSlot takes a slot without blocking, returning a release
// func and whether it succeeded. It never queues: waiting for a slot would let
// the backlog grow, which is the thing being defended against. The decision is
// made before any reply is read, so it cannot depend on whether the instance
// exists — a nonexistent id and another account's id throttle identically.
func acquireAbsenceProofSlot() (release func(), ok bool) {
	select {
	case absenceProofSlots <- struct{}{}:
		return func() { <-absenceProofSlots }, true
	default:
		return nil, false
	}
}

// defaultDescribeFanoutTimeout is the hard deadline the describe fan-out waits
// for node replies before treating the sweep as incomplete.
const defaultDescribeFanoutTimeout = 3 * time.Second

// describeResponderGrace is how long the fan-out keeps listening after every
// configured node has answered. Sized well above the sub-millisecond spread
// between node replies, so a loaded node still lands inside it.
const describeResponderGrace = 150 * time.Millisecond

// describeConfig holds the tunable knobs of the describe fan-out.
type describeConfig struct {
	fanoutTimeout time.Duration
}

// DescribeOption customises a describe fan-out. Production callers pass none
// and get defaultDescribeFanoutTimeout.
type DescribeOption func(*describeConfig)

// WithFanoutTimeout overrides the fan-out deadline.
func WithFanoutTimeout(d time.Duration) DescribeOption {
	return func(c *describeConfig) { c.fanoutTimeout = d }
}

// DescribeInstances fans out to all nodes via NATS and aggregates the results.
// It is lenient: partial results from a slow or unreachable node are returned
// without error, and an explicitly named instance ID that is simply absent
// from the aggregate is not an error either — the caller sees an empty list.
// That silence is required by internal callers (the IMDS instance lookup, the
// RDS instance-state resolver, the EKS control-plane reconciler) which treat
// "not found" as "not currently running/present" rather than a fault.
// The customer-facing gateway action uses DescribeInstancesChecked instead,
// which adds the InvalidInstanceID.NotFound assertion AWS callers expect.
// Callers that must not act on a partial view at all (the quota reconcile)
// use DescribeInstancesForReconcile.
func DescribeInstances(ctx context.Context, input *ec2.DescribeInstancesInput, natsConn *nats.Conn, expectedNodes int, accountID string, opts ...DescribeOption) (*ec2.DescribeInstancesOutput, error) {
	reservations, _, firstClient4xx, err := gatherInstances(ctx, input, natsConn, expectedNodes, nil, accountID, opts...)
	if err != nil {
		return nil, err
	}
	// Propagate a deterministic 4xx only when nothing was collected (fan-out + KV).
	if firstClient4xx != "" && len(reservations) == 0 {
		return nil, errors.New(firstClient4xx)
	}
	return &ec2.DescribeInstancesOutput{Reservations: reservations}, nil
}

// DescribeInstancesChecked is the customer-facing variant served by the
// gateway's DescribeInstances action. On top of DescribeInstances' aggregation,
// it asserts InvalidInstanceID.NotFound for any explicitly named instance ID
// absent from the result — but only when the sweep is provably complete. A
// partial sweep stays silent, the same as DescribeInstances: a node timing
// out during the fan-out must never turn into a false NotFound for an
// instance that actually exists on that node. A --filters-only query (no
// explicit IDs) is never affected and keeps returning an empty list with no
// error.
//
// nodeIDs is the configured cluster node set. When non-empty, completeness
// for the InstanceIds assertion is judged by responder identity — the
// configured nodes whose frames actually decoded — rather than by
// expectedNodes, and the fan-out collects to its full deadline rather than
// exiting once data arrives, since absence cannot be proven from a prefix of
// the replies. An empty or unset nodeIDs falls back to expectedNodes, which a
// caller that does not supply one leaves at 0 — never complete, so the
// assertion is never made.
func DescribeInstancesChecked(ctx context.Context, input *ec2.DescribeInstancesInput, natsConn *nats.Conn, expectedNodes int, nodeIDs []string, accountID string, opts ...DescribeOption) (*ec2.DescribeInstancesOutput, error) {
	reservations, complete, firstClient4xx, err := gatherInstances(ctx, input, natsConn, expectedNodes, nodeIDs, accountID, opts...)
	if err != nil {
		return nil, err
	}
	if firstClient4xx != "" && len(reservations) == 0 {
		return nil, errors.New(firstClient4xx)
	}

	if len(input.InstanceIds) > 0 && complete {
		found := make(map[string]bool)
		for _, res := range reservations {
			for _, inst := range res.Instances {
				if inst != nil && inst.InstanceId != nil {
					found[*inst.InstanceId] = true
				}
			}
		}
		for _, id := range input.InstanceIds {
			if id != nil && !found[*id] {
				return nil, errors.New(awserrors.ErrorInvalidInstanceIDNotFound)
			}
		}
	}

	return &ec2.DescribeInstancesOutput{Reservations: reservations}, nil
}

// DescribeInstancesForReconcile is the strict variant, for a caller that may act
// only on a provably whole view. complete is true only when the sweep observed
// every expected node and both instance buckets; a partial sweep — a node down,
// a timed-out fan-out, or a failed bucket query — returns complete=false so the
// caller can decline to act rather than act on a short count. The quota vCPU
// sweep reads the instance record space instead of this; the retype gate and the
// volume delete guard are what still call it.
func DescribeInstancesForReconcile(ctx context.Context, input *ec2.DescribeInstancesInput, natsConn *nats.Conn, expectedNodes int, accountID string) (reservations []*ec2.Reservation, complete bool, err error) {
	reservations, complete, _, err = gatherInstances(ctx, input, natsConn, expectedNodes, nil, accountID)
	return reservations, complete, err
}

// gatherInstances runs the running-instance fan-out plus the stopped/terminated
// KV bucket queries and aggregates every reservation. firstClient4xx carries
// the first deterministic 4xx for the lenient caller to surface when nothing
// was collected.
//
// With nodeIDs empty, complete is judged the historical way: a success frame
// from all expectedNodes, without timing out. With nodeIDs set, complete is
// judged by responder identity instead — see DescribeInstancesChecked — and an
// explicit-ID request collects to its deadline under CollectUntilDeadline; a
// request with no explicit IDs stays on CollectServeData regardless, since
// nothing there is being proved.
func gatherInstances(ctx context.Context, input *ec2.DescribeInstancesInput, natsConn *nats.Conn, expectedNodes int, nodeIDs []string, accountID string, opts ...DescribeOption) (reservations []*ec2.Reservation, complete bool, firstClient4xx string, err error) {
	cfg := describeConfig{fanoutTimeout: defaultDescribeFanoutTimeout}
	for _, opt := range opts {
		opt(&cfg)
	}

	jsonData, err := json.Marshal(input)
	if err != nil {
		slog.ErrorContext(ctx, "DescribeInstances: Failed to marshal input", "err", err)
		return nil, false, "", fmt.Errorf("failed to marshal input: %w", err)
	}

	// The buckets run alongside the fan-out rather than after it, so a stopped
	// or terminated instance can settle the collection too. Queried on queue
	// groups, so one responder each.
	var kvMu sync.Mutex
	var kvWg sync.WaitGroup
	bucketsOK := true
	var bucketReservations []*ec2.Reservation
	settler := newInstanceSettler(input.InstanceIds)
	for _, topic := range []string{"ec2.DescribeStoppedInstances", "ec2.DescribeTerminatedInstances"} {
		kvWg.Add(1)
		go func(topic string) {
			defer kvWg.Done()
			reservations, ok := queryInstanceBucket(ctx, natsConn, topic, jsonData, accountID)
			settler.observe(reservations)
			kvMu.Lock()
			defer kvMu.Unlock()
			if !ok {
				bucketsOK = false
			}
			bucketReservations = append(bucketReservations, reservations...)
		}(topic)
	}

	identity := len(nodeIDs) > 0
	gatherOpts := utils.GatherOpts{Timeout: cfg.fanoutTimeout, AccountID: accountID}
	if identity {
		gatherOpts.ExpectedResponders = len(nodeIDs)
		if len(input.InstanceIds) > 0 {
			release, ok := acquireAbsenceProofSlot()
			if !ok {
				kvWg.Wait()
				slog.WarnContext(ctx, "DescribeInstances: absence proof budget exhausted, throttling",
					"limit", maxConcurrentAbsenceProofs)
				return nil, false, "", errors.New(awserrors.ErrorRequestLimitExceeded)
			}
			defer release()

			// Only the path that could assert absence collects past the point
			// where every node has answered; a filters-only listing may still
			// exit on the first frames.
			gatherOpts.Mode = utils.CollectUntilDeadline
			gatherOpts.ResponderGrace = describeResponderGrace
			gatherOpts.Settled = settler.settled
		}
	} else {
		gatherOpts.ExpectedNodes = expectedNodes
	}

	frames, sum, err := utils.Gather(ctx, natsConn, "ec2.DescribeInstances", jsonData, gatherOpts)
	if err != nil {
		kvWg.Wait()
		return nil, false, "", err
	}

	var allReservations []*ec2.Reservation
	var fanoutComplete bool
	if identity {
		allReservations, fanoutComplete = judgeIdentityCompleteness(ctx, frames, sum, nodeIDs)
	} else {
		for _, frame := range frames {
			var nodeOutput ec2.DescribeInstancesOutput
			if json.Unmarshal(frame.Data, &nodeOutput) == nil && nodeOutput.Reservations != nil {
				allReservations = append(allReservations, nodeOutput.Reservations...)
			}
		}
		// The fan-out is complete only when every expected node answered with a
		// success frame and the deadline was not hit; an error frame or a missing
		// node leaves the view partial.
		fanoutComplete = expectedNodes > 0 && !sum.TimedOut && sum.Successes >= expectedNodes
	}

	kvWg.Wait()
	allReservations = append(allReservations, bucketReservations...)

	slog.InfoContext(ctx, "DescribeInstances: Aggregated response", "total_reservations", len(allReservations))
	return allReservations, fanoutComplete && bucketsOK, sum.FirstClient4xx, nil
}

// instanceSettler tracks which of the requested instance ids have turned up,
// across both the running-instance fan-out and the stopped and terminated
// bucket queries that run alongside it. Once none are outstanding there is
// nothing left to prove and collection can stop without waiting out the
// responder grace window.
//
// A bucket answer is only consulted when the next fan-out frame arrives, so it
// shortens the wait only when it lands first. Absence collects past this point,
// and an instance the caller cannot see is filtered out of every source, so it
// takes the absence path too — the two NotFound answers stay indistinguishable
// in time.
type instanceSettler struct {
	mu       sync.Mutex
	wanted   map[string]bool
	consumed int
}

func newInstanceSettler(instanceIDs []*string) *instanceSettler {
	wanted := map[string]bool{}
	for _, id := range instanceIDs {
		if id != nil {
			wanted[*id] = true
		}
	}
	return &instanceSettler{wanted: wanted}
}

// observe records ids found outside the fan-out. Safe to call concurrently
// with settled, which is what the bucket goroutines do.
func (s *instanceSettler) observe(reservations []*ec2.Reservation) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.forget(reservations)
}

// settled is the Gather predicate. It decodes only the frames it has not seen
// before, so a large fan-out does not re-decode its whole backlog per frame.
func (s *instanceSettler) settled(frames []utils.Frame, _ utils.Summary) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for ; s.consumed < len(frames); s.consumed++ {
		var out ec2.DescribeInstancesOutput
		if json.Unmarshal(frames[s.consumed].Data, &out) != nil {
			continue
		}
		s.forget(out.Reservations)
	}
	return len(s.wanted) == 0
}

// forget drops every id present in reservations. Caller holds the lock.
func (s *instanceSettler) forget(reservations []*ec2.Reservation) {
	for _, res := range reservations {
		if res == nil {
			continue
		}
		for _, inst := range res.Instances {
			if inst != nil && inst.InstanceId != nil {
				delete(s.wanted, *inst.InstanceId)
			}
		}
	}
}

// judgeIdentityCompleteness decodes every frame and builds ValidResponders —
// the configured nodes whose payload actually decoded as DescribeInstancesOutput.
// A node with nil Reservations still decoded, so it is a valid responder, not
// an invalid one; only a failed decode withholds a node from the set.
// Completeness requires ValidResponders to cover nodeIDs and fails closed on
// any ambiguity: a frame with no node ID, a node that answered as both a
// success and an error, or a node whose repeated frames disagreed.
func judgeIdentityCompleteness(ctx context.Context, frames []utils.Frame, sum utils.Summary, nodeIDs []string) (reservations []*ec2.Reservation, complete bool) {
	validResponders := map[string]bool{}
	for _, frame := range frames {
		var nodeOutput ec2.DescribeInstancesOutput
		if json.Unmarshal(frame.Data, &nodeOutput) != nil {
			if frame.NodeID != "" {
				slog.WarnContext(ctx, "DescribeInstances: frame did not decode as DescribeInstancesOutput", "node", frame.NodeID)
			}
			continue
		}
		if frame.NodeID != "" {
			validResponders[frame.NodeID] = true
		}
		reservations = append(reservations, nodeOutput.Reservations...)
	}

	// An unattributable responder makes completeness permanently unsatisfiable
	// for identity mode: it can never be resolved to a missing node, so every
	// sweep it touches stays incomplete forever, which silently suppresses
	// every absence 404 while looking like a healthy conservative answer.
	if sum.Unidentified > 0 {
		slog.WarnContext(ctx, "DescribeInstances: fan-out received frames with no node ID header",
			"unidentified_frames", sum.Unidentified)
	}

	ambiguous := sum.Unidentified > 0 || len(sum.ConflictNodes) > 0 || sum.CapHit
	var bothSets []string
	for node := range sum.SuccessResponders {
		if sum.ErrorResponders[node] {
			ambiguous = true
			bothSets = append(bothSets, node)
		}
	}

	var missing, erroring []string
	for _, id := range nodeIDs {
		if !validResponders[id] {
			missing = append(missing, id)
		}
		if sum.ErrorResponders[id] {
			erroring = append(erroring, id)
		}
	}

	complete = len(nodeIDs) > 0 && len(missing) == 0 && !ambiguous
	// A settled early exit leaves nodes unheard by design, so the shortfall is
	// the caller's own doing and says nothing about cluster health.
	if !complete && !sum.SettledEarly {
		slog.InfoContext(ctx, "DescribeInstances: fan-out incomplete",
			"missing_nodes", missing, "erroring_nodes", erroring, "conflict_nodes", len(sum.ConflictNodes),
			"unidentified_frames", sum.Unidentified, "nodes_answering_as_both", bothSets, "cap_hit", sum.CapHit)
	}
	return reservations, complete
}

// EnrichInstanceProfileIDs resolves IamInstanceProfile.Id for every instance
// that carries only an ARN. Results are cached per ARN to avoid repeated RPCs.
// Misses are warn-logged and leave Id empty (graceful degradation for deleted profiles).
// Safe to call with a nil output or nil IAMService (no-op).
func EnrichInstanceProfileIDs(out *ec2.DescribeInstancesOutput, iamSvc handlers_iam.IAMService, accountID string) {
	if out == nil || iamSvc == nil {
		return
	}
	cache := map[string]string{} // ARN → ID; "" = miss
	for _, res := range out.Reservations {
		if res == nil {
			continue
		}
		for _, inst := range res.Instances {
			if inst == nil || inst.IamInstanceProfile == nil {
				continue
			}
			arn := aws.StringValue(inst.IamInstanceProfile.Arn)
			if arn == "" {
				continue
			}
			id, cached := cache[arn]
			if !cached {
				profile, err := iamSvc.ResolveInstanceProfile(accountID, arn)
				if err != nil || profile == nil {
					slog.Warn("DescribeInstances: failed to resolve instance profile ID",
						"arn", arn, "err", err)
					cache[arn] = ""
				} else {
					id = profile.InstanceProfileID
					cache[arn] = id
				}
			}
			if id != "" {
				inst.IamInstanceProfile.Id = aws.String(id)
			}
		}
	}
}

// queryInstanceBucket queries a single describe topic and returns its
// reservations. ok is false when the query failed (request error or error
// payload), so a reconcile caller can treat the sweep as incomplete rather than
// silently dropping the bucket's instances.
func queryInstanceBucket(ctx context.Context, natsConn *nats.Conn, topic string, jsonData []byte, accountID string) (reservations []*ec2.Reservation, ok bool) {
	reqMsg := nats.NewMsg(topic)
	reqMsg.Data = jsonData
	reqMsg.Header.Set(utils.AccountIDHeader, accountID)
	utils.InjectTraceContext(ctx, reqMsg.Header)
	msg, err := natsConn.RequestMsg(reqMsg, 3*time.Second)
	if err != nil {
		slog.WarnContext(ctx, "DescribeInstances: Failed to query instance bucket", "topic", topic, "err", err)
		return nil, false
	}
	if responseError, parseErr := utils.ValidateErrorPayload(msg.Data); parseErr != nil {
		slog.WarnContext(ctx, "DescribeInstances: Instance bucket query returned error", "topic", topic, "code", responseError.Code)
		return nil, false
	}
	var output ec2.DescribeInstancesOutput
	if err := json.Unmarshal(msg.Data, &output); err != nil {
		slog.ErrorContext(ctx, "DescribeInstances: Failed to unmarshal instance bucket response", "topic", topic, "err", err)
		return nil, false
	}
	if len(output.Reservations) > 0 {
		slog.InfoContext(ctx, "DescribeInstances: Collected reservations from bucket", "topic", topic, "count", len(output.Reservations))
	}
	return output.Reservations, true
}
