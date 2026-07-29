package handlers_rds

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/nats-io/nats.go"
)

// The control-plane half of the rds-2b command channel. The agent side is a
// long poll over the gateway; this publishes on the bus subject that poll is
// subscribed to and waits for the reply the agent carries back on its next one.

// Command types. rds-5b adds grow-filesystem and rds-8 the snapshot quiesce.
const (
	CommandSetPassword = "set-password"
	CommandApplyParams = "apply-params"
	CommandStopEngine  = "stop-engine"
)

// Parameter names carried on a command. They are AWS-shaped rather than
// engine-shaped: the agent maps them onto ALTER ROLE or a config write.
const (
	CommandParamMasterUsername     = "MasterUsername"
	CommandParamMasterUserPassword = "MasterUserPassword"
)

// Per-command budgets. A password apply is one statement; a parameter apply
// rewrites a config file and reloads; a graceful engine stop has to checkpoint,
// which is bounded by the dirty set rather than by anything the caller knows.
const (
	setPasswordTimeout = 30 * time.Second
	applyParamsTimeout = 60 * time.Second
	stopEngineTimeout  = 120 * time.Second

	// The channel is a live subscription rather than a durable queue (D8), so a
	// command published between two of the agent's polls reaches nobody.
	// Re-publishing on this interval closes that gap without queueing anything.
	commandRepublishEvery = 2 * time.Second
)

// The agent could not be reached, or did not answer inside the command's
// budget. Callers that can degrade — the graceful engine stop — check for this;
// callers that must not, like a password rotation, surface it as retryable.
var ErrCommandUnreachable = errors.New("rds: the instance agent did not answer the command")

// Sets the engine's master password live. It is never persisted anywhere: an
// unreachable agent fails loudly rather than leaving cleartext queued for
// later (D8).
func (s *Service) setMasterPassword(ctx context.Context, accountID, dbInstanceIdentifier, username, password string) error {
	_, err := s.issueCommand(ctx, accountID, dbInstanceIdentifier, CommandSetPassword, setPasswordTimeout, []Parameter{
		{Name: CommandParamMasterUsername, Value: username},
		{Name: CommandParamMasterUserPassword, Value: password},
	})
	return err
}

// Writes the resolved parameter set into the engine's config and reloads it.
// Returns the settings the engine accepted but will not apply until it
// restarts, which is what RebootDBInstance then clears (D16).
func (s *Service) applyParameters(ctx context.Context, accountID, dbInstanceIdentifier string, params []Parameter) ([]string, error) {
	reply, err := s.issueCommand(ctx, accountID, dbInstanceIdentifier, CommandApplyParams, applyParamsTimeout, params)
	if err != nil {
		return nil, err
	}
	return parsePendingRestart(reply.Message), nil
}

// Shuts the engine down cleanly so the data volume is checkpointed before the
// VM stops. Callers treat a failure as degradation, not as an error.
func (s *Service) stopEngine(ctx context.Context, accountID, dbInstanceIdentifier string) error {
	_, err := s.issueCommand(ctx, accountID, dbInstanceIdentifier, CommandStopEngine, stopEngineTimeout, nil)
	return err
}

// Publishes one directive and blocks until the agent replies to that command
// ID, the budget expires, or ctx is cancelled. A reply reporting failure is
// returned as an error carrying the agent's message, so the caller does not
// have to inspect the status itself.
func (s *Service) issueCommand(ctx context.Context, accountID, dbInstanceIdentifier, commandType string, timeout time.Duration, params []Parameter) (*CommandReply, error) {
	if s.nc == nil {
		return nil, errors.New("rds: nil nats connection")
	}
	issuedAt := time.Now().UTC()
	cmd := Command{
		CommandID:  uuid.NewString(),
		Type:       commandType,
		Parameters: params,
		IssuedAt:   &issuedAt,
	}
	data, err := json.Marshal(cmd)
	if err != nil {
		return nil, err
	}

	// Subscribed before the first publish, so a reply cannot land in the gap
	// between the command going out and the issuer starting to listen.
	sub, err := s.nc.SubscribeSync(BusCommandReplySubject(accountID, dbInstanceIdentifier))
	if err != nil {
		return nil, fmt.Errorf("rds: subscribe command replies for %s: %w", dbInstanceIdentifier, err)
	}
	defer func() {
		if err := sub.Unsubscribe(); err != nil {
			slog.DebugContext(ctx, "rds: command reply unsubscribe failed", "dbInstance", dbInstanceIdentifier, "err", err)
		}
	}()

	deadlineCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	subject := BusCommandSubject(accountID, dbInstanceIdentifier)

	for {
		if err := s.nc.Publish(subject, data); err != nil {
			return nil, fmt.Errorf("rds: publish %s command to %s: %w", commandType, dbInstanceIdentifier, err)
		}
		reply, err := waitForReply(deadlineCtx, sub, cmd.CommandID)
		if err != nil {
			return nil, err
		}
		if reply == nil {
			// Nothing yet: re-publish in case the agent was between polls.
			continue
		}
		if reply.Status != CommandStatusSucceeded {
			return nil, fmt.Errorf("rds: %s command failed on %s: %s", commandType, dbInstanceIdentifier, reply.Message)
		}
		slog.DebugContext(ctx, "rds: agent command completed",
			"dbInstance", dbInstanceIdentifier, "type", commandType, "commandId", cmd.CommandID)
		return reply, nil
	}
}

// Returns (nil, nil) when the republish interval elapsed with no matching
// reply, so the caller re-publishes; ErrCommandUnreachable once the whole
// budget is gone. Replies for other command IDs are stale answers to an earlier
// issuer and are skipped.
func waitForReply(ctx context.Context, sub *nats.Subscription, commandID string) (*CommandReply, error) {
	window, cancel := context.WithTimeout(ctx, commandRepublishEvery)
	defer cancel()

	for {
		msg, err := sub.NextMsgWithContext(window)
		if err != nil {
			// The caller giving up is its own failure, not an unreachable agent.
			if errors.Is(ctx.Err(), context.Canceled) {
				return nil, ctx.Err()
			}
			if ctx.Err() != nil {
				return nil, ErrCommandUnreachable
			}
			// Only the republish window closed.
			return nil, nil
		}
		var reply CommandReply
		if err := json.Unmarshal(msg.Data, &reply); err != nil {
			slog.Debug("rds: undecodable command reply dropped", "err", err)
			continue
		}
		if reply.CommandID == commandID {
			return &reply, nil
		}
	}
}

// The agent reports the settings still awaiting a restart as a comma-separated
// list in its reply message, since CommandReply carries no structured payload.
func parsePendingRestart(message string) []string {
	var pending []string
	for name := range strings.SplitSeq(message, ",") {
		if trimmed := strings.TrimSpace(name); trimmed != "" {
			pending = append(pending, trimmed)
		}
	}
	return pending
}
