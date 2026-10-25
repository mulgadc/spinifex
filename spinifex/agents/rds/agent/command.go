package agent

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"

	rdsinstance "github.com/mulgadc/spinifex/spinifex/domains/rds/instance"
)

// Executes one control-plane directive inside the guest and returns the message
// reported back to the issuer.
type commandHandler func(ctx context.Context, cmd rdsinstance.Command) (string, error)

// A registry rather than a dispatch switch, so guest operations — password
// apply, parameter reload, storage grow, snapshot quiesce — add an entry each.
type commandRegistry map[string]commandHandler

// An unregistered type is replied to as unsupported, so a control plane ahead
// of the guest gets a clear answer rather than a timeout.
func newCommandRegistry(engine engineOps, storage storageOps) commandRegistry {
	return commandRegistry{
		rdsinstance.CommandSetPassword: func(ctx context.Context, cmd rdsinstance.Command) (string, error) {
			params := commandParams(cmd)
			return "", engine.SetPassword(ctx,
				params[rdsinstance.CommandParamMasterUsername],
				params[rdsinstance.CommandParamMasterUserPassword])
		},
		rdsinstance.CommandApplyParams: func(ctx context.Context, cmd rdsinstance.Command) (string, error) {
			pending, err := engine.ApplyParameters(ctx, cmd.Parameters)
			if err != nil {
				return "", err
			}
			// The reply carries no structured payload, so the settings still
			// awaiting a restart come back as the message itself.
			return strings.Join(pending, ","), nil
		},
		rdsinstance.CommandStopEngine: func(ctx context.Context, cmd rdsinstance.Command) (string, error) {
			return "", engine.Stop(ctx)
		},
		rdsinstance.CommandGrowFilesystem: func(ctx context.Context, cmd rdsinstance.Command) (string, error) {
			return storage.GrowFilesystem(ctx)
		},
		rdsinstance.CommandQuiesce: func(ctx context.Context, cmd rdsinstance.Command) (string, error) {
			params := commandParams(cmd)
			hold, err := quiesceHoldFrom(params[rdsinstance.CommandParamQuiesceDeadlineSeconds])
			if err != nil {
				return "", err
			}
			if err := engine.Quiesce(ctx, params[rdsinstance.CommandParamQuiesceLabel], hold); err != nil {
				return "", err
			}
			// After the hold, so what is flushed is the engine's checkpointed
			// state. Its own files are not the risk — those it repairs from the
			// WAL — but the parameter file beside them it never opens.
			return "", storage.SyncDataMount(ctx)
		},
		rdsinstance.CommandUnquiesce: func(ctx context.Context, cmd rdsinstance.Command) (string, error) {
			return "", engine.Unquiesce(ctx)
		},
	}
}

// The control plane's hold deadline. A missing or unreadable one is refused
// rather than defaulted: a hold the guest chose the length of would not be the
// bound the control plane is relying on.
func quiesceHoldFrom(value string) (time.Duration, error) {
	seconds, err := strconv.Atoi(strings.TrimSpace(value))
	if err != nil || seconds <= 0 {
		return 0, fmt.Errorf("%s must be a positive number of seconds, got %q",
			rdsinstance.CommandParamQuiesceDeadlineSeconds, value)
	}
	return time.Duration(seconds) * time.Second, nil
}

// A later value wins, so a malformed command carrying a parameter twice cannot
// leave the handler acting on the first of two conflicting values.
func commandParams(cmd rdsinstance.Command) map[string]string {
	params := make(map[string]string, len(cmd.Parameters))
	for _, p := range cmd.Parameters {
		params[p.Name] = p.Value
	}
	return params
}

// Spaces retries after a failed poll, so a broken channel is not re-polled at
// line rate for as long as it stays broken.
const pollErrorBackoff = 5 * time.Second

// The agent's command channel: a long poll that carries back the replies for
// whatever the previous poll delivered.
type commander struct {
	cp       controlPlane
	id       identity
	registry commandRegistry
	wait     time.Duration
	// Cleared only once a poll has carried them, so a failed poll re-delivers
	// rather than dropping the result of work the guest actually did.
	pending []rdsinstance.CommandReply
}

func newCommander(cp controlPlane, registry commandRegistry, wait time.Duration) *commander {
	if wait <= 0 {
		wait = defaultPollWait
	}
	return &commander{cp: cp, registry: registry, wait: wait}
}

func (c *commander) Run(ctx context.Context) {
	for {
		if ctx.Err() != nil {
			return
		}
		commands, err := c.cp.PollCommands(ctx, c.id, c.pending, c.wait)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			slog.Warn("rds-agent: command poll failed", "err", err)
			select {
			case <-ctx.Done():
				return
			case <-time.After(pollErrorBackoff):
			}
			continue
		}
		c.pending = nil

		for _, cmd := range commands {
			c.pending = append(c.pending, c.execute(ctx, cmd))
		}
	}
}

// Every command gets a reply: the issuer is blocked on this command ID, so a
// silent drop costs it a full timeout.
func (c *commander) execute(ctx context.Context, cmd rdsinstance.Command) rdsinstance.CommandReply {
	handler, ok := c.registry[cmd.Type]
	if !ok {
		slog.Warn("rds-agent: unsupported command", "commandID", cmd.CommandID, "type", cmd.Type)
		return failedReply(cmd, fmt.Sprintf("unsupported command type %q", cmd.Type))
	}

	slog.Info("rds-agent: executing command", "commandID", cmd.CommandID, "type", cmd.Type)
	message, err := handler(ctx, cmd)
	if err != nil {
		slog.Error("rds-agent: command failed", "commandID", cmd.CommandID, "type", cmd.Type, "err", err)
		return failedReply(cmd, err.Error())
	}
	return rdsinstance.CommandReply{
		CommandID: cmd.CommandID,
		Status:    rdsinstance.CommandStatusSucceeded,
		Message:   message,
	}
}

func failedReply(cmd rdsinstance.Command, message string) rdsinstance.CommandReply {
	return rdsinstance.CommandReply{
		CommandID: cmd.CommandID,
		Status:    rdsinstance.CommandStatusFailed,
		Message:   message,
	}
}
