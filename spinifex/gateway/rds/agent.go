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
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
)

// Long-poll bounds for PollDBCommands. The ceiling keeps a poll inside the
// gateway's request timeout; the floor stops a misbehaving agent turning the
// long poll into a busy loop.
const (
	minPollWait     = 1 * time.Second
	maxPollWait     = 20 * time.Second
	defaultPollWait = 20 * time.Second
)

// The DB instance a caller has been proven to be.
type agentIdentity struct {
	AccountID            string
	DBInstanceIdentifier string
	InstanceID           string
	VMGeneration         int64
}

// requestedID, from the request body, is only ever used to reject a mismatch —
// the authoritative identifier comes from the index, so an agent cannot act on
// another instance by asking to.
func authorizeAgent(ctx context.Context, nc *nats.Conn, caller Caller, requestedID string) (*agentIdentity, error) {
	// Re-run at the handler even though the gateway gate already ran it: this is
	// the check that must hold, and it must not depend on a caller remembering.
	if err := requireAgentPrincipal(ctx, caller); err != nil {
		return nil, err
	}

	// IMDS instance-role credentials set RoleSessionName to the internal EC2
	// instance ID, the reverse-index key. The role ARN proves the caller is an
	// RDS VM; the session name only says which one.
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
		VMGeneration:         entry.VMGeneration,
	}, nil
}

// Reads JetStream directly rather than adding a NATS round trip, matching the
// OIDC discovery and client-token paths, because this runs on every agent call.
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

// The identifier is accepted but authoritative identity comes from the gate.
type RegisterDBInstanceInput struct {
	DBInstanceIdentifier string `locationName:"DBInstanceIdentifier"`
	AgentVersion         string `locationName:"AgentVersion"`
	EngineVersion        string `locationName:"EngineVersion"`
}

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

type SubmitDBStateChangeInput struct {
	DBInstanceIdentifier string `locationName:"DBInstanceIdentifier"`
	EngineHealth         string `locationName:"EngineHealth"`
	EngineVersion        string `locationName:"EngineVersion"`
	Message              string `locationName:"Message"`
}

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

type GetDBBootstrapConfigInput struct {
	DBInstanceIdentifier string `locationName:"DBInstanceIdentifier"`
}

// Serves boot material, including the master password on the first fetch only.
func GetDBBootstrapConfig(ctx context.Context, input *GetDBBootstrapConfigInput, nc *nats.Conn, caller Caller) (any, error) {
	id, err := authorizeAgent(ctx, nc, caller, input.DBInstanceIdentifier)
	if err != nil {
		return nil, err
	}
	return handlers_rds.NewNATSService(nc).GetDBBootstrapConfig(ctx, &handlers_rds.GetDBBootstrapConfigInput{
		DBInstanceIdentifier: id.DBInstanceIdentifier,
		InstanceID:           id.InstanceID,
		VMGeneration:         id.VMGeneration,
	}, id.AccountID)
}

// Carries results for commands delivered on an earlier poll and asks for the
// next one, matching the ECS ack-on-poll shape.
type PollDBCommandsInput struct {
	DBInstanceIdentifier string                      `locationName:"DBInstanceIdentifier"`
	WaitTimeSeconds      int64                       `locationName:"WaitTimeSeconds"`
	Replies              []handlers_rds.CommandReply `locationName:"Replies" locationNameList:"member"`
}

// At most one command per poll: each is a discrete guest operation the agent
// must complete and report before the next is safe to issue.
type PollDBCommandsOutput struct {
	Commands []handlers_rds.Command `locationName:"Commands" locationNameList:"member"`
}

// The command channel is a live subscription, not a durable queue, so a
// set-password that cannot reach the agent fails loudly rather than queueing
// cleartext in KV.
func PollDBCommands(ctx context.Context, input *PollDBCommandsInput, nc *nats.Conn, caller Caller) (any, error) {
	id, err := authorizeAgent(ctx, nc, caller, input.DBInstanceIdentifier)
	if err != nil {
		return nil, err
	}
	if nc == nil {
		return nil, errors.New(awserrors.ErrorServerInternal)
	}

	// Replies are published before the subscription is opened so a reply is not
	// delayed by the poll's own wait. Publication is part of poll success: the
	// agent retains and retries this entire batch when the poll fails.
	if err := publishReplies(nc, id, input.Replies); err != nil {
		slog.ErrorContext(ctx, "RDS: publish command replies failed",
			"dbInstanceIdentifier", id.DBInstanceIdentifier, "err", err)
		return nil, errors.New(awserrors.ErrorServerInternal)
	}

	sub, err := nc.QueueSubscribeSync(
		handlers_rds.BusCommandSubject(id.AccountID, id.DBInstanceIdentifier),
		handlers_rds.CommandQueueGroup)
	if err != nil {
		slog.ErrorContext(ctx, "RDS: command subscribe failed", "dbInstanceIdentifier", id.DBInstanceIdentifier, "err", err)
		return nil, errors.New(awserrors.ErrorServerInternal)
	}
	// Unsubscribing on every exit path stops an abandoned subscription
	// swallowing a later command from a queue group nobody is reading.
	defer func() {
		if err := sub.Unsubscribe(); err != nil {
			slog.DebugContext(ctx, "RDS: command unsubscribe failed", "err", err)
		}
	}()

	pollCtx, cancel := context.WithTimeout(ctx, pollWait(input.WaitTimeSeconds))
	defer cancel()

	msg, err := sub.NextMsgWithContext(pollCtx)
	if err != nil {
		// The context ending the wait is the steady state: the window closed with
		// no command, or the agent hung up.
		if pollCtx.Err() != nil {
			return &PollDBCommandsOutput{}, nil
		}
		// Anything else means the channel is broken, not idle. Reporting that as
		// an empty poll would have the agent re-poll at line rate against a
		// channel no command can arrive on.
		slog.ErrorContext(ctx, "RDS: command poll failed",
			"dbInstanceIdentifier", id.DBInstanceIdentifier, "err", err)
		return nil, errors.New(awserrors.ErrorServerInternal)
	}

	var cmd handlers_rds.Command
	if err := json.Unmarshal(msg.Data, &cmd); err != nil {
		slog.ErrorContext(ctx, "RDS: undecodable command dropped", "dbInstanceIdentifier", id.DBInstanceIdentifier, "err", err)
		return nil, errors.New(awserrors.ErrorServerInternal)
	}
	return &PollDBCommandsOutput{Commands: []handlers_rds.Command{cmd}}, nil
}

func pollWait(requested int64) time.Duration {
	if requested <= 0 {
		return defaultPollWait
	}
	return min(max(time.Duration(requested)*time.Second, minPollWait), maxPollWait)
}

func publishReplies(nc *nats.Conn, id *agentIdentity, replies []handlers_rds.CommandReply) error {
	subject := handlers_rds.BusCommandReplySubject(id.AccountID, id.DBInstanceIdentifier)
	for _, reply := range replies {
		if reply.CommandID == "" {
			continue
		}
		data, err := json.Marshal(reply)
		if err != nil {
			return fmt.Errorf("marshal command reply %s: %w", reply.CommandID, err)
		}
		if err := nc.Publish(subject, data); err != nil {
			return fmt.Errorf("publish command reply %s: %w", reply.CommandID, err)
		}
	}
	return nil
}
