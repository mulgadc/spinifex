package handlers_rds

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/nats-io/nats.go"
)

// The control-plane half of the rds-2b command channel. The agent side is a
// long poll over the gateway; this publishes on the bus subject that poll is
// subscribed to and waits for the reply the agent carries back on its next one.

const (
	CommandSetPassword = "set-password"
	CommandApplyParams = "apply-params"
	CommandStopEngine  = "stop-engine"
	// Extends the in-guest filesystem onto a data volume the control plane has
	// already grown (D12).
	CommandGrowFilesystem = "grow-filesystem"
	// Holds the engine at a checkpoint for the length of a snapshot, and releases
	// it again (D10). The hold is a session the agent keeps open, so the pair is
	// two commands rather than one with a duration.
	CommandQuiesce   = "quiesce"
	CommandUnquiesce = "unquiesce"
)

// Parameter names carried on a command. They are AWS-shaped rather than
// engine-shaped: the agent maps them onto ALTER ROLE or a config write.
const (
	CommandParamMasterUsername     = "MasterUsername"
	CommandParamMasterUserPassword = "MasterUserPassword"

	// The backup label the engine records, and how long the agent holds the
	// quiesce before releasing it unasked.
	CommandParamQuiesceLabel           = "QuiesceLabel"
	CommandParamQuiesceDeadlineSeconds = "QuiesceDeadlineSeconds"
)

// Per-command budgets. A password apply is one statement; a parameter apply
// rewrites a config file and reloads; a graceful engine stop has to checkpoint,
// which is bounded by the dirty set rather than by anything the caller knows.
const (
	setPasswordTimeout = 30 * time.Second
	applyParamsTimeout = 60 * time.Second
	stopEngineTimeout  = 120 * time.Second
	// growpart plus an online resize2fs/xfs_growfs. Both scale with the
	// filesystem's metadata rather than with the volume, so this is generous
	// rather than proportional to the grow.
	growFilesystemTimeout = 180 * time.Second

	// A quiesce forces an immediate checkpoint, which is bounded by the dirty set
	// the same way a graceful stop is; the release is one statement.
	quiesceTimeout   = 120 * time.Second
	unquiesceTimeout = 60 * time.Second

	// How long the agent holds the backup session before releasing it unasked. It
	// covers the drain and the whole ec2.CreateSnapshot round trip with room to
	// spare, so a control plane that dies mid-snapshot costs the engine this much
	// time in backup mode rather than the rest of its life.
	quiesceHold = 6 * time.Minute

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

// Extends the guest's filesystem onto the already-grown data volume. Issued
// once the agent is back rather than during boot: both ext4 and XFS grow while
// mounted, so this needs no ordering against the engine start (D12).
func (s *Service) growFilesystem(ctx context.Context, accountID, dbInstanceIdentifier string) error {
	_, err := s.issueCommand(ctx, accountID, dbInstanceIdentifier, CommandGrowFilesystem, growFilesystemTimeout, nil)
	return err
}

// Holds the engine at a checkpoint so the data volume can be snapshotted at a
// consistent point. The agent keeps the backup session open until the release
// below, or until the deadline expires — so a control plane that dies here does
// not leave the engine in backup mode indefinitely (D10).
func (s *Service) quiesceEngine(ctx context.Context, accountID, dbInstanceIdentifier, label string) error {
	_, err := s.issueCommand(ctx, accountID, dbInstanceIdentifier, CommandQuiesce, quiesceTimeout, []Parameter{
		{Name: CommandParamQuiesceLabel, Value: label},
		{Name: CommandParamQuiesceDeadlineSeconds, Value: strconv.Itoa(int(quiesceHold.Seconds()))},
	})
	return err
}

// Releases the held backup session. Idempotent on the agent side, so a release
// that races the deadline is a success rather than an error the caller has to
// interpret.
func (s *Service) unquiesceEngine(ctx context.Context, accountID, dbInstanceIdentifier string) error {
	_, err := s.issueCommand(ctx, accountID, dbInstanceIdentifier, CommandUnquiesce, unquiesceTimeout, nil)
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
