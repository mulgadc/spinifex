package main

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	handlers_rds "github.com/mulgadc/spinifex/spinifex/handlers/rds"
)

// commandHandler executes one control-plane directive inside the guest and
// returns the message reported back to the issuer.
type commandHandler func(ctx context.Context, cmd handlers_rds.Command) (string, error)

// commandRegistry maps a command type to the handler that performs it. It is a
// registry rather than a dispatch switch so the phases that own the concrete
// guest operations — live password apply and parameter reload, storage grow,
// snapshot quiesce — add an entry instead of editing a growing switch.
type commandRegistry map[string]commandHandler

// newCommandRegistry returns the directives this build can execute. It is
// deliberately empty: this phase delivers the channel, and each operation lands
// with the phase that owns it. An unregistered type is replied to as
// unsupported, so a control plane ahead of the guest gets a clear answer rather
// than a timeout.
func newCommandRegistry() commandRegistry {
	return commandRegistry{}
}

// pollErrorBackoff spaces retries after a failed poll. Without it a broken
// channel — a gateway that is down, credentials that no longer authorize — would
// be re-polled at line rate for as long as it stayed broken.
const pollErrorBackoff = 5 * time.Second

// commander runs the agent's command channel: a long poll that carries back the
// replies for whatever the previous poll delivered.
type commander struct {
	cp       controlPlane
	id       identity
	registry commandRegistry
	wait     time.Duration
	// pending holds replies not yet accepted by a poll. They are cleared only
	// once a poll has carried them, so a failed poll re-delivers rather than
	// dropping the result of work the guest actually did.
	pending []handlers_rds.CommandReply
}

func newCommander(cp controlPlane, registry commandRegistry, wait time.Duration) *commander {
	if wait <= 0 {
		wait = defaultPollWait
	}
	return &commander{cp: cp, registry: registry, wait: wait}
}

// Run polls for directives until ctx is cancelled, executing each and queuing
// its reply for the next poll.
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

// execute runs one command and turns the outcome into a reply. Every command
// gets one: the issuer is blocked on this command ID, so a silent drop costs it
// a full timeout and tells it nothing about why.
func (c *commander) execute(ctx context.Context, cmd handlers_rds.Command) handlers_rds.CommandReply {
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
	return handlers_rds.CommandReply{
		CommandID: cmd.CommandID,
		Status:    handlers_rds.CommandStatusSucceeded,
		Message:   message,
	}
}

func failedReply(cmd handlers_rds.Command, message string) handlers_rds.CommandReply {
	return handlers_rds.CommandReply{
		CommandID: cmd.CommandID,
		Status:    handlers_rds.CommandStatusFailed,
		Message:   message,
	}
}
