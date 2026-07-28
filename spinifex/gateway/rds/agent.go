package gateway_rds

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/mulgadc/spinifex/spinifex/awserrors"
	handlers_rds "github.com/mulgadc/spinifex/spinifex/handlers/rds"
	"github.com/mulgadc/spinifex/spinifex/utils"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
)

// InstanceRoleName is the instance profile the RDS system VMs carry. Only a
// session assumed from this role may call the internal agent actions.
const InstanceRoleName = "rdsInstanceRole"

// Long-poll bounds for PollDBCommands. The ceiling keeps a poll inside the
// gateway's own request timeout; the floor stops a misbehaving agent turning the
// long poll into a busy loop.
const (
	minPollWait     = 1 * time.Second
	maxPollWait     = 20 * time.Second
	defaultPollWait = 20 * time.Second
)

// agentIdentity is the DB instance a caller has been proven to be.
type agentIdentity struct {
	AccountID            string
	DBInstanceIdentifier string
	InstanceID           string
}

// authorizeAgent resolves the caller to exactly one DB instance, or rejects it.
//
// The gate is deliberately coarse at this phase: system account, rdsInstanceRole,
// and a reverse-index hit for the session's instance ID. The binding to the
// caller's attested instance identity is tightened later.
//
// requestedID is the DBInstanceIdentifier from the request body. It is only ever
// used to reject a mismatch — the authoritative identifier comes from the index,
// so an agent cannot act on another instance by asking to.
func authorizeAgent(ctx context.Context, nc *nats.Conn, caller Caller, requestedID string) (*agentIdentity, error) {
	if caller.PrincipalType != principalTypeAssumedRole ||
		caller.AccountID != utils.GlobalAccountID ||
		caller.RoleName != InstanceRoleName ||
		caller.SessionName == "" {
		slog.DebugContext(ctx, "RDS: internal action rejected for non-agent caller",
			"principalType", caller.PrincipalType, "accountID", caller.AccountID, "roleName", caller.RoleName)
		return nil, errors.New(awserrors.ErrorAccessDenied)
	}

	// IMDS instance-role credentials set RoleSessionName to the internal EC2
	// instance ID, which is the reverse-index key. The role ARN is what proves
	// the caller is an RDS VM; the session name only says which one.
	entry, err := lookupInstanceIndex(ctx, nc, caller.SessionName)
	if err != nil {
		slog.ErrorContext(ctx, "RDS: instance-index lookup failed", "instanceID", caller.SessionName, "err", err)
		return nil, errors.New(awserrors.ErrorServerInternal)
	}
	if entry == nil {
		slog.DebugContext(ctx, "RDS: no instance-index entry for caller", "instanceID", caller.SessionName)
		return nil, errors.New(awserrors.ErrorAccessDenied)
	}

	if requestedID != "" && requestedID != entry.DBInstanceIdentifier {
		slog.WarnContext(ctx, "RDS: agent requested another instance",
			"callerInstanceID", caller.SessionName, "resolved", entry.DBInstanceIdentifier, "requested", requestedID)
		return nil, errors.New(awserrors.ErrorAccessDenied)
	}

	return &agentIdentity{
		AccountID:            entry.AccountID,
		DBInstanceIdentifier: entry.DBInstanceIdentifier,
		InstanceID:           caller.SessionName,
	}, nil
}

// lookupInstanceIndex reads the rds-system reverse index directly from
// JetStream. The gateway reads KV directly here rather than adding a NATS
// round trip, matching the OIDC discovery and client-token paths, because this
// runs on every agent call.
func lookupInstanceIndex(ctx context.Context, nc *nats.Conn, instanceID string) (*handlers_rds.InstanceIndexEntry, error) {
	if nc == nil {
		return nil, errors.New("gateway NATS connection not initialised")
	}
	js, err := jetstream.New(nc)
	if err != nil {
		return nil, err
	}
	kv, err := js.KeyValue(ctx, handlers_rds.KVBucketRDSSystem)
	if err != nil {
		// No bucket means no RDS instance has ever been created, so no caller
		// can be an agent. That is a denial, not an internal failure.
		if errors.Is(err, jetstream.ErrBucketNotFound) {
			return nil, nil
		}
		return nil, err
	}
	entry, err := kv.Get(ctx, handlers_rds.InstanceIndexKey(instanceID))
	if errors.Is(err, jetstream.ErrKeyNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out handlers_rds.InstanceIndexEntry
	if err := json.Unmarshal(entry.Value(), &out); err != nil {
		return nil, fmt.Errorf("unmarshal instance index %s: %w", instanceID, err)
	}
	return &out, nil
}

// RegisterDBInstanceInput is the agent's registration request. The identifier is
// accepted but authoritative identity comes from the gate.
type RegisterDBInstanceInput struct {
	DBInstanceIdentifier string `locationName:"DBInstanceIdentifier"`
	AgentVersion         string `locationName:"AgentVersion"`
	EngineVersion        string `locationName:"EngineVersion"`
}

// RegisterDBInstance records an agent's boot and refreshes its liveness.
func RegisterDBInstance(ctx context.Context, input *RegisterDBInstanceInput, nc *nats.Conn, caller Caller) (any, error) {
	id, err := authorizeAgent(ctx, nc, caller, input.DBInstanceIdentifier)
	if err != nil {
		return nil, err
	}
	return handlers_rds.NewNATSService(nc).RegisterDBInstance(ctx, &handlers_rds.RegisterDBInstanceInput{
		DBInstanceIdentifier: id.DBInstanceIdentifier,
		InstanceID:           id.InstanceID,
		AgentVersion:         input.AgentVersion,
		EngineVersion:        input.EngineVersion,
	}, id.AccountID)
}

// SubmitDBStateChangeInput is the agent's periodic state-and-liveness beat.
type SubmitDBStateChangeInput struct {
	DBInstanceIdentifier string `locationName:"DBInstanceIdentifier"`
	EngineHealth         string `locationName:"EngineHealth"`
	EngineVersion        string `locationName:"EngineVersion"`
	Message              string `locationName:"Message"`
}

// SubmitDBStateChange records engine health and refreshes liveness.
func SubmitDBStateChange(ctx context.Context, input *SubmitDBStateChangeInput, nc *nats.Conn, caller Caller) (any, error) {
	id, err := authorizeAgent(ctx, nc, caller, input.DBInstanceIdentifier)
	if err != nil {
		return nil, err
	}
	return handlers_rds.NewNATSService(nc).SubmitDBStateChange(ctx, &handlers_rds.SubmitDBStateChangeInput{
		DBInstanceIdentifier: id.DBInstanceIdentifier,
		InstanceID:           id.InstanceID,
		EngineHealth:         handlers_rds.EngineHealth(input.EngineHealth),
		EngineVersion:        input.EngineVersion,
		Message:              input.Message,
	}, id.AccountID)
}

// GetDBBootstrapConfigInput is the agent's first call on every boot.
type GetDBBootstrapConfigInput struct {
	DBInstanceIdentifier string `locationName:"DBInstanceIdentifier"`
}

// GetDBBootstrapConfig serves the agent's boot material, including the master
// password on the first fetch only.
func GetDBBootstrapConfig(ctx context.Context, input *GetDBBootstrapConfigInput, nc *nats.Conn, caller Caller) (any, error) {
	id, err := authorizeAgent(ctx, nc, caller, input.DBInstanceIdentifier)
	if err != nil {
		return nil, err
	}
	return handlers_rds.NewNATSService(nc).GetDBBootstrapConfig(ctx, &handlers_rds.GetDBBootstrapConfigInput{
		DBInstanceIdentifier: id.DBInstanceIdentifier,
		InstanceID:           id.InstanceID,
	}, id.AccountID)
}

// PollDBCommandsInput carries results for commands delivered on an earlier poll
// and asks for the next one. Replies ride the poll rather than using their own
// action, matching the ECS ack-on-poll shape.
type PollDBCommandsInput struct {
	DBInstanceIdentifier string                      `locationName:"DBInstanceIdentifier"`
	WaitTimeSeconds      int64                       `locationName:"WaitTimeSeconds"`
	Replies              []handlers_rds.CommandReply `locationName:"Replies" locationNameList:"member"`
}

// PollDBCommandsOutput carries at most one command per poll. Commands are
// delivered one at a time because each is a discrete guest operation the agent
// must complete and report before the next is safe to issue.
type PollDBCommandsOutput struct {
	Commands []handlers_rds.Command `locationName:"Commands" locationNameList:"member"`
}

// PollDBCommands is the agent's long poll for control-plane directives.
//
// The channel is a live subscription, not a durable queue: a command published
// while no agent is polling is lost and its issuer times out. That is the
// intended contract — it is what lets a set-password that cannot reach the agent
// fail loudly rather than leave cleartext queued in KV.
func PollDBCommands(ctx context.Context, input *PollDBCommandsInput, nc *nats.Conn, caller Caller) (any, error) {
	id, err := authorizeAgent(ctx, nc, caller, input.DBInstanceIdentifier)
	if err != nil {
		return nil, err
	}
	if nc == nil {
		return nil, errors.New(awserrors.ErrorServerInternal)
	}

	// Replies are published before the subscription is opened so a reply is not
	// delayed by the poll's own wait.
	publishReplies(ctx, nc, id, input.Replies)

	sub, err := nc.QueueSubscribeSync(
		handlers_rds.BusCommandSubject(id.AccountID, id.DBInstanceIdentifier),
		handlers_rds.CommandQueueGroup)
	if err != nil {
		slog.ErrorContext(ctx, "RDS: command subscribe failed", "dbInstanceIdentifier", id.DBInstanceIdentifier, "err", err)
		return nil, errors.New(awserrors.ErrorServerInternal)
	}
	// Unsubscribing on every exit path is what keeps a poll from leaving a
	// subscription behind that would swallow a later command from a queue group
	// nobody is reading.
	defer func() {
		if err := sub.Unsubscribe(); err != nil {
			slog.DebugContext(ctx, "RDS: command unsubscribe failed", "err", err)
		}
	}()

	pollCtx, cancel := context.WithTimeout(ctx, pollWait(input.WaitTimeSeconds))
	defer cancel()

	msg, err := sub.NextMsgWithContext(pollCtx)
	if err != nil {
		// An empty poll is the steady state, not an error: the agent simply
		// polls again.
		return &PollDBCommandsOutput{}, nil
	}

	var cmd handlers_rds.Command
	if err := json.Unmarshal(msg.Data, &cmd); err != nil {
		slog.ErrorContext(ctx, "RDS: undecodable command dropped", "dbInstanceIdentifier", id.DBInstanceIdentifier, "err", err)
		return nil, errors.New(awserrors.ErrorServerInternal)
	}
	return &PollDBCommandsOutput{Commands: []handlers_rds.Command{cmd}}, nil
}

// pollWait clamps the caller's requested wait to the long-poll window.
func pollWait(requested int64) time.Duration {
	if requested <= 0 {
		return defaultPollWait
	}
	return min(max(time.Duration(requested)*time.Second, minPollWait), maxPollWait)
}

// publishReplies forwards the agent's command results to the issuer. Failures
// are logged rather than returned: a lost reply times the issuer out, which is
// the same outcome as a lost command and better than failing the whole poll.
func publishReplies(ctx context.Context, nc *nats.Conn, id *agentIdentity, replies []handlers_rds.CommandReply) {
	subject := handlers_rds.BusCommandReplySubject(id.AccountID, id.DBInstanceIdentifier)
	for _, reply := range replies {
		if reply.CommandID == "" {
			continue
		}
		data, err := json.Marshal(reply)
		if err != nil {
			slog.ErrorContext(ctx, "RDS: marshal command reply", "commandID", reply.CommandID, "err", err)
			continue
		}
		if err := nc.Publish(subject, data); err != nil {
			slog.ErrorContext(ctx, "RDS: publish command reply", "commandID", reply.CommandID, "err", err)
		}
	}
}
