package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/mulgadc/spinifex/internal/gwsign"
	"github.com/mulgadc/spinifex/internal/rdsgw"
	handlers_rds "github.com/mulgadc/spinifex/spinifex/handlers/rds"
)

// Overridable via -ldflags "-X main.version=...".
var version = "dev"

// The gateway resolves the authoritative DB instance from the request's
// credentials, so DBInstanceIdentifier is an assertion to be checked, not
// trusted.
type identity struct {
	DBInstanceIdentifier string
	AgentVersion         string
	EngineVersion        string
}

// Every method is a SigV4-signed Query request, so the NATS bus stays
// host-internal.
type controlPlane interface {
	Register(ctx context.Context, id identity) (*handlers_rds.RegisterDBInstanceOutput, error)
	SubmitState(ctx context.Context, id identity, health handlers_rds.EngineHealth, message string) (*handlers_rds.SubmitDBStateChangeOutput, error)
	// Returns Mode=initialize with the master password for as long as a payload
	// staged for this VM generation is unacknowledged, and Mode=attach after.
	GetBootstrapConfig(ctx context.Context, id identity) (*handlers_rds.GetDBBootstrapConfigOutput, error)
	// Reports that PostgreSQL durably applied the staged password, which is what
	// lets the control plane destroy it.
	AcknowledgeBootstrap(ctx context.Context, id identity, pending *pendingBootstrap) error
	PollCommands(ctx context.Context, id identity, replies []handlers_rds.CommandReply, wait time.Duration) ([]handlers_rds.Command, error)
}

// Bounds a register, heartbeat or bootstrap request. The long poll sets its own
// deadline from the wait it asked the gateway to hold.
const callTimeout = 15 * time.Second

// Keeps the poll's deadline past the requested wait, so a gateway answering at
// the end of its window is not cut off by the client.
const pollSlack = 10 * time.Second

type gatewayControlPlane struct {
	client *rdsgw.Client
}

var _ controlPlane = (*gatewayControlPlane)(nil)

// Signs with the instance-role credentials the SDK chain resolves from IMDS,
// against the pinned gateway CA.
func newGatewayControlPlane(cfg config) (*gatewayControlPlane, error) {
	signer, err := gwsign.NewIMDS(context.Background(), cfg.Region)
	if err != nil {
		return nil, fmt.Errorf("build IMDS signer: %w", err)
	}
	// The client timeout is the ceiling for the longest call, the long poll;
	// shorter calls carry their own deadline.
	client, err := rdsgw.New(cfg.GatewayURL, cfg.GatewayCA, signer, cfg.Region, cfg.PollWait+pollSlack)
	if err != nil {
		return nil, err
	}
	return &gatewayControlPlane{client: client}, nil
}

func (g *gatewayControlPlane) Register(ctx context.Context, id identity) (*handlers_rds.RegisterDBInstanceOutput, error) {
	params := url.Values{}
	setIfPresent(params, "DBInstanceIdentifier", id.DBInstanceIdentifier)
	setIfPresent(params, "AgentVersion", id.AgentVersion)
	setIfPresent(params, "EngineVersion", id.EngineVersion)

	ctx, cancel := context.WithTimeout(ctx, callTimeout)
	defer cancel()

	var out handlers_rds.RegisterDBInstanceOutput
	if err := g.client.Call(ctx, "RegisterDBInstance", params, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func (g *gatewayControlPlane) SubmitState(ctx context.Context, id identity, health handlers_rds.EngineHealth, message string) (*handlers_rds.SubmitDBStateChangeOutput, error) {
	params := url.Values{"EngineHealth": {string(health)}}
	setIfPresent(params, "DBInstanceIdentifier", id.DBInstanceIdentifier)
	setIfPresent(params, "EngineVersion", id.EngineVersion)
	setIfPresent(params, "Message", message)

	ctx, cancel := context.WithTimeout(ctx, callTimeout)
	defer cancel()

	var out handlers_rds.SubmitDBStateChangeOutput
	if err := g.client.Call(ctx, "SubmitDBStateChange", params, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func (g *gatewayControlPlane) GetBootstrapConfig(ctx context.Context, id identity) (*handlers_rds.GetDBBootstrapConfigOutput, error) {
	params := url.Values{}
	setIfPresent(params, "DBInstanceIdentifier", id.DBInstanceIdentifier)

	ctx, cancel := context.WithTimeout(ctx, callTimeout)
	defer cancel()

	var out handlers_rds.GetDBBootstrapConfigOutput
	if err := g.client.Call(ctx, "GetDBBootstrapConfig", params, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func (g *gatewayControlPlane) AcknowledgeBootstrap(ctx context.Context, id identity, pending *pendingBootstrap) error {
	params := url.Values{
		"PayloadId":    {pending.payloadID},
		"VMGeneration": {strconv.FormatInt(pending.vmGeneration, 10)},
	}
	setIfPresent(params, "DBInstanceIdentifier", id.DBInstanceIdentifier)
	setIfPresent(params, "DataVolumeId", pending.dataVolumeID)

	ctx, cancel := context.WithTimeout(ctx, callTimeout)
	defer cancel()

	var out handlers_rds.AcknowledgeDBBootstrapOutput
	if err := g.client.Call(ctx, "AcknowledgeDBBootstrap", params, &out); err != nil {
		return err
	}
	if !out.Acknowledged {
		return fmt.Errorf("the control plane did not acknowledge bootstrap payload %s", pending.payloadID)
	}
	return nil
}

// Declared here so the in-guest binary does not link the server side of the API.
type pollDBCommandsOutput struct {
	Commands []handlers_rds.Command `xml:"Commands>member"`
}

func (g *gatewayControlPlane) PollCommands(ctx context.Context, id identity, replies []handlers_rds.CommandReply, wait time.Duration) ([]handlers_rds.Command, error) {
	params := url.Values{"WaitTimeSeconds": {strconv.FormatInt(int64(wait.Seconds()), 10)}}
	setIfPresent(params, "DBInstanceIdentifier", id.DBInstanceIdentifier)
	// Replies ride the poll rather than a separate action, so replying and
	// asking for the next command are one round trip.
	for i, reply := range replies {
		member := fmt.Sprintf("Replies.member.%d.", i+1)
		params.Set(member+"CommandId", reply.CommandID)
		params.Set(member+"Status", reply.Status)
		setIfPresent(params, member+"Message", reply.Message)
	}

	ctx, cancel := context.WithTimeout(ctx, wait+pollSlack)
	defer cancel()

	var out pollDBCommandsOutput
	if err := g.client.Call(ctx, "PollDBCommands", params, &out); err != nil {
		return nil, err
	}
	return out.Commands, nil
}

// Only sets a parameter that has a value, so the gateway can tell "no
// assertion" from "asserted as blank".
func setIfPresent(params url.Values, key, value string) {
	if value != "" {
		params.Set(key, value)
	}
}

type Agent struct {
	cfg             config
	id              identity
	cp              controlPlane
	probe           *engineProbe
	engine          engine
	handoffWriter   func(string, *handlers_rds.GetDBBootstrapConfigOutput) error
	dataMountWaiter func(context.Context, string, string) error

	// Set by bootstrap when the fetch replayed a staged payload; nil leaves the
	// acknowledgement step a no-op, which is every attach boot.
	pending *pendingBootstrap

	hb    *heartbeater
	cmd   *commander
	guard *paramGuard
}

// Assembles from already-built parts, so tests can pass fakes; New builds the
// production control plane and delegates here.
func newAgent(cfg config, cp controlPlane, probe *engineProbe) (*Agent, error) {
	if cfg.MountsFile == "" {
		cfg.MountsFile = defaultMountsFile
	}
	id := identity{
		DBInstanceIdentifier: cfg.DBInstanceIdentifier,
		AgentVersion:         version,
		EngineVersion:        cfg.EngineVersion,
	}
	a := &Agent{
		cfg: cfg, id: id, cp: cp, probe: probe,
		handoffWriter: writeHandoff, dataMountWaiter: waitForDataMount,
	}
	eng, err := newEngine(cfg, execCommandRunner, execSessionRunner, probe)
	if err != nil {
		return nil, err
	}
	a.engine = eng
	a.hb = newHeartbeater(cp, probe, a.engine, handlers_rds.HeartbeatInterval)
	a.cmd = newCommander(cp, newCommandRegistry(a.engine, newGuestStorage(cfg, execCommandRunner)), cfg.PollWait)
	a.guard = newParamGuard(a.engine, probe, cp)
	return a, nil
}

// Does not wait for IMDS: the register loop rides out a datapath still coming up.
func New(cfg config) (*Agent, error) {
	if cfg.GatewayURL == "" {
		return nil, fmt.Errorf("no gateway URL configured (RDS_GATEWAY_URL)")
	}
	cp, err := newGatewayControlPlane(cfg)
	if err != nil {
		return nil, err
	}
	probe, err := newProbe(cfg, execProbeRunner)
	if err != nil {
		return nil, err
	}
	return newAgent(cfg, cp, probe)
}

func (a *Agent) Run(ctx context.Context) error {
	// Named before the first dial: when register cannot reach the gateway, the
	// address it is dialing is what separates a mgmt NIC that came up without one
	// from a control plane that is genuinely down.
	slog.Info("rds-agent: starting",
		"gatewayURL", a.cfg.GatewayURL, "dbInstanceIdentifier", a.id.DBInstanceIdentifier,
		"agentVersion", a.id.AgentVersion)

	// Registration comes first: its response is the agent's identity.
	if err := a.register(ctx); err != nil {
		return err
	}
	a.hb.id, a.cmd.id, a.guard.id = a.id, a.id, a.id

	// Ahead of the handoff, so rds-init never unblocks and no datadir is touched.
	// An agent running on the wrong image would initialise one engine over
	// another's data, coming up healthy and empty beside data nothing references.
	if err := a.refuseEngineMismatch(ctx); err != nil {
		return err
	}

	// Beating before the bootstrap keeps a stuck boot visible as a live VM with
	// a down engine rather than as silence.
	go a.hb.Run(ctx)

	if err := a.bootstrap(ctx); err != nil {
		return err
	}

	// The handoff must land before this wait: rds-datadir needs it to identify
	// and authorize the volume. Commands and the parameter guard wait because
	// their durable state belongs on that mount, never on the disposable boot disk.
	if err := a.dataMountWaiter(ctx, a.cfg.MountsFile, a.cfg.DataMount); err != nil {
		// A shutdown cancels this wait; main treats the wrapped context.Canceled
		// as a clean stop, exactly as it does for register and bootstrap above.
		return fmt.Errorf("wait for data mount %s: %w", a.cfg.DataMount, err)
	}

	// The receipt it waits on lives on that mount, so this cannot run any earlier.
	// A cancelled wait is a clean stop here too.
	if err := a.acknowledgeBootstrap(ctx); err != nil {
		return err
	}

	// Directives are only meaningful against a bootstrapped, mounted engine.
	go a.cmd.Run(ctx)

	// Started after the handoff, so the window it measures covers the engine's
	// own start rather than the time rds-init spent waiting for this agent.
	go a.guard.Run(ctx)

	<-ctx.Done()
	return nil
}

// The engine cloud-init says this VM was launched as, against the engine the
// image bakes. An empty expectation is no assertion rather than a disagreement:
// a VM launched before the control plane emitted RDS_ENGINE carries none.
func (a *Agent) refuseEngineMismatch(ctx context.Context) error {
	expected := strings.ToLower(strings.TrimSpace(a.cfg.Engine))
	if expected == "" || expected == a.cfg.BakedEngine {
		return nil
	}

	message := fmt.Sprintf("this VM was launched as engine %q but its image bakes %q; refusing to bootstrap",
		expected, a.cfg.BakedEngine)
	slog.ErrorContext(ctx, "rds-agent: "+message)
	// Reported before returning, so the control plane learns why the instance
	// never came up rather than only that it never did.
	if _, err := a.cp.SubmitState(ctx, a.id, handlers_rds.EngineHealthUnhealthy, message); err != nil {
		slog.ErrorContext(ctx, "rds-agent: reporting the engine mismatch failed", "err", err)
	}
	return errors.New(message)
}

const dataMountPollInterval = time.Second

// waitForDataMount blocks the stateful agent loops until the kernel reports the
// configured data mount. Registration, heartbeat, and bootstrap run before it.
func waitForDataMount(ctx context.Context, mountsFile, target string) error {
	ticker := time.NewTicker(dataMountPollInterval)
	defer ticker.Stop()

	for {
		mounted, err := mountTableContains(mountsFile, target)
		if err != nil {
			slog.WarnContext(ctx, "rds-agent: could not read mount table while waiting for data volume",
				"mountsFile", mountsFile, "dataMount", target, "err", err)
		} else if mounted {
			slog.InfoContext(ctx, "rds-agent: data volume mount is ready", "dataMount", target)
			return nil
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

func mountTableContains(path, target string) (bool, error) {
	f, err := os.Open(path)
	if err != nil {
		return false, err
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) >= 2 && unescapeMountField(fields[1]) == target {
			return true, nil
		}
	}
	if err := scanner.Err(); err != nil {
		return false, err
	}
	return false, nil
}

// Linux mount tables encode whitespace and backslashes as octal escapes.
func unescapeMountField(field string) string {
	replacer := strings.NewReplacer(`\134`, `\`, `\040`, " ", `\011`, "\t", `\012`, "\n")
	return replacer.Replace(field)
}

// Boot-retry bounds. Tight to start, since the usual cause is a control plane
// still creating this instance's record, and capped so an outage does not turn
// a booting fleet into a retry storm.
const (
	retryMin = 1 * time.Second
	retryMax = 30 * time.Second

	// With the production backoff, attempt five is about 15 seconds into the
	// failure and still precedes the first 30-second heartbeat.
	retryErrorAttempt = 5
)

// Wrapping an error in this stops retryObserved. For the failures the control
// plane decides from its own record, no retry can change the answer, and looping
// on one only keeps the caller blocked.
var errRetryTerminal = errors.New("terminal failure")

// Never gives up on its own: an agent that stopped trying would leave the
// control plane with no signal at all.
func retry(ctx context.Context, what string, fn func(context.Context) error) error {
	return retryObserved(ctx, what, fn, nil)
}

// onFailure lets boot-critical callers publish the latest error while the retry
// remains blocked. It must not block because it runs on the retry path itself.
func retryObserved(ctx context.Context, what string, fn func(context.Context) error, onFailure func(error)) error {
	delay := retryMin
	for attempt := 1; ; attempt++ {
		err := fn(ctx)
		if err == nil {
			return nil
		}
		if ctx.Err() != nil {
			return fmt.Errorf("%s: %w", what, ctx.Err())
		}
		if onFailure != nil {
			onFailure(err)
		}
		if errors.Is(err, errRetryTerminal) {
			slog.ErrorContext(ctx, "rds-agent: "+what+" failed terminally, not retrying", "err", err)
			return fmt.Errorf("%s: %w", what, err)
		}
		if attempt >= retryErrorAttempt {
			slog.ErrorContext(ctx, "rds-agent: "+what+" still failing, retrying",
				"attempt", attempt, "retryIn", delay, "err", err)
		} else {
			slog.WarnContext(ctx, "rds-agent: "+what+" failed, retrying",
				"attempt", attempt, "retryIn", delay, "err", err)
		}

		select {
		case <-ctx.Done():
			return fmt.Errorf("%s: %w", what, ctx.Err())
		case <-time.After(delay):
		}
		delay = min(delay*2, retryMax)
	}
}
