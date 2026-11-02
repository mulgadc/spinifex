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

// How many engine commands may wait behind the one running. Past this a command
// is refused at once rather than queued behind work its issuer will not outlive.
const serialQueueDepth = 16

// The budget for a type the control plane does not issue. Such a command is
// answered as unsupported before it could use any of it.
const fallbackCommandBudget = 30 * time.Second

// The only command that runs beside the serial lane. It releases the backup hold
// an engine command can be blocked on, so it must never queue behind one.
func outOfBand(commandType string) bool {
	return commandType == rdsinstance.CommandUnquiesce
}

// A command waiting its turn on the serial lane, with the moment its issuer
// stops waiting for it.
type queuedCommand struct {
	cmd      rdsinstance.Command
	deadline time.Time
}

// The agent's command channel: a long poll that carries back the replies for
// whatever earlier polls delivered. Engine commands run one at a time on a single
// lane; the release of a backup hold runs beside it, so it cannot be kept out.
type commander struct {
	cp       controlPlane
	id       identity
	registry commandRegistry
	wait     time.Duration
	// The control plane's budget per type, measured from receipt so the agent never
	// gives up before its issuer. IssuedAt is not compared: the guest clock is not
	// trusted against the host's.
	budget func(commandType string) time.Duration
	serial chan queuedCommand

	mu sync.Mutex
	// Cleared only once a poll has carried them, so a failed poll re-delivers
	// rather than dropping the result of work the guest actually did.
	pending []rdsinstance.CommandReply
	// Queued or running, so a re-published copy is not queued a second time.
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
		budget:   controlPlaneBudget,
		serial:   make(chan queuedCommand, serialQueueDepth),
		inFlight: make(map[string]struct{}),
	}
}

func controlPlaneBudget(commandType string) time.Duration {
	if budget, ok := rdsinstance.CommandBudget(commandType); ok {
		return budget
	}
	return fallbackCommandBudget
}

func (c *commander) Run(ctx context.Context) {
	defer c.running.Wait()
	c.running.Go(func() { c.serveSerial(ctx) })
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

// Routes cmd to its lane unless it is already queued, running or answered: the
// issuer re-publishes until it has a reply, so the same ID arrives again.
func (c *commander) dispatch(ctx context.Context, cmd rdsinstance.Command) {
	c.mu.Lock()
	_, queued := c.inFlight[cmd.CommandID]
	if queued || slices.Contains(c.completed, cmd.CommandID) {
		c.mu.Unlock()
		slog.Debug("rds-agent: duplicate command ignored", "commandID", cmd.CommandID, "type", cmd.Type)
		return
	}
	c.inFlight[cmd.CommandID] = struct{}{}
	c.mu.Unlock()

	q := queuedCommand{cmd: cmd, deadline: time.Now().Add(c.budget(cmd.Type))}
	if outOfBand(cmd.Type) {
		c.running.Go(func() { c.runQueued(ctx, q) })
		return
	}
	select {
	case c.serial <- q:
	default:
		c.finish(cmd, failedReply(cmd, "the agent is busy; too many commands are waiting"))
	}
}

func (c *commander) serveSerial(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case q := <-c.serial:
			c.runQueued(ctx, q)
		}
	}
}

// Runs a command inside what is left of its issuer's budget. One whose budget
// ran out while it waited is answered as expired and never touches the engine,
// since its issuer has already reported it failed.
func (c *commander) runQueued(ctx context.Context, q queuedCommand) {
	if !time.Now().Before(q.deadline) {
		slog.Warn("rds-agent: command expired before it ran", "commandID", q.cmd.CommandID, "type", q.cmd.Type)
		c.finish(q.cmd, failedReply(q.cmd, "the command expired before the agent could run it"))
		return
	}
	cmdCtx, cancel := context.WithDeadline(ctx, q.deadline)
	defer cancel()
	c.finish(q.cmd, c.execute(cmdCtx, q.cmd))
}

func (c *commander) finish(cmd rdsinstance.Command, reply rdsinstance.CommandReply) {
	c.mu.Lock()
	defer c.mu.Unlock()
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
