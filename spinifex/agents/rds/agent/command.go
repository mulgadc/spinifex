package agent

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strconv"
	"strings"
	"sync"
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

// How many finished command IDs are remembered, so a copy the issuer re-published
// while the command ran is not executed a second time.
const completedCommandMemory = 64

// The agent's command channel: a long poll that carries back the replies for
// whatever earlier polls delivered. Commands run on their own goroutines so one
// the engine is blocking cannot keep out the command that would unblock it.
type commander struct {
	cp       controlPlane
	id       identity
	registry commandRegistry
	wait     time.Duration

	mu sync.Mutex
	// Cleared only once a poll has carried them, so a failed poll re-delivers
	// rather than dropping the result of work the guest actually did.
	pending  []rdsinstance.CommandReply
	inFlight map[string]struct{}
	// Oldest first, bounded by completedCommandMemory.
	completed []string
	// Ends the poll in progress, so a reply that finished during it rides the
	// next one rather than waiting out the window. Nil between polls.
	interruptPoll func()
	interrupted   bool
	running       sync.WaitGroup
}

func newCommander(cp controlPlane, registry commandRegistry, wait time.Duration) *commander {
	if wait <= 0 {
		wait = defaultPollWait
	}
	return &commander{
		cp:       cp,
		registry: registry,
		wait:     wait,
		inFlight: make(map[string]struct{}),
	}
}

func (c *commander) Run(ctx context.Context) {
	defer c.running.Wait()
	for {
		if ctx.Err() != nil {
			return
		}
		commands, sent, err := c.poll(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			if errors.Is(err, errReplyReady) {
				continue
			}
			slog.Warn("rds-agent: command poll failed", "err", err)
			select {
			case <-ctx.Done():
				return
			case <-time.After(pollErrorBackoff):
			}
			continue
		}
		c.mu.Lock()
		c.pending = c.pending[sent:]
		c.mu.Unlock()

		for _, cmd := range commands {
			c.dispatch(ctx, cmd)
		}
	}
}

// Reports that a poll was ended early so a finished reply can ride the next one.
var errReplyReady = errors.New("a command reply is ready")

// One long poll carrying every reply ready when it starts. It is cut short when
// another reply becomes ready, so a finished command is never answered a whole
// poll window late. Returns how many pending replies the poll carried.
func (c *commander) poll(ctx context.Context) ([]rdsinstance.Command, int, error) {
	pollCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	c.mu.Lock()
	replies := slices.Clone(c.pending)
	c.interruptPoll, c.interrupted = cancel, false
	c.mu.Unlock()

	commands, err := c.cp.PollCommands(pollCtx, c.id, replies, c.wait)

	c.mu.Lock()
	interrupted := c.interrupted
	c.interruptPoll = nil
	c.mu.Unlock()
	if err != nil {
		if interrupted && ctx.Err() == nil {
			return nil, 0, errReplyReady
		}
		return nil, 0, err
	}
	return commands, len(replies), nil
}

// Starts cmd unless it is already running or already answered: the issuer
// re-publishes until it has a reply, so the same ID arrives again while it runs.
func (c *commander) dispatch(ctx context.Context, cmd rdsinstance.Command) {
	c.mu.Lock()
	_, running := c.inFlight[cmd.CommandID]
	if running || slices.Contains(c.completed, cmd.CommandID) {
		c.mu.Unlock()
		slog.Debug("rds-agent: duplicate command ignored", "commandID", cmd.CommandID, "type", cmd.Type)
		return
	}
	c.inFlight[cmd.CommandID] = struct{}{}
	c.mu.Unlock()

	c.running.Go(func() {
		reply := c.execute(ctx, cmd)

		c.mu.Lock()
		delete(c.inFlight, cmd.CommandID)
		c.completed = append(c.completed, cmd.CommandID)
		if len(c.completed) > completedCommandMemory {
			c.completed = c.completed[len(c.completed)-completedCommandMemory:]
		}
		c.pending = append(c.pending, reply)
		if c.interruptPoll != nil {
			c.interruptPoll()
			c.interrupted = true
		}
		c.mu.Unlock()
	})
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
