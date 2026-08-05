package main

import (
	"context"
	"fmt"
	"log/slog"
	"net/url"
	"strconv"
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
	// The first call of an instance's life returns Mode=initialize with the
	// master password; every later call returns Mode=attach without it.
	GetBootstrapConfig(ctx context.Context, id identity) (*handlers_rds.GetDBBootstrapConfigOutput, error)
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
	cfg           config
	id            identity
	cp            controlPlane
	probe         *engineProbe
	engine        *postgresEngine
	handoffWriter func(string, *handlers_rds.GetDBBootstrapConfigOutput) error

	hb    *heartbeater
	cmd   *commander
	guard *paramGuard
}

// Assembles from already-built parts, so tests can pass fakes; New builds the
// production control plane and delegates here.
func newAgent(cfg config, cp controlPlane, probe *engineProbe) *Agent {
	id := identity{
		DBInstanceIdentifier: cfg.DBInstanceIdentifier,
		AgentVersion:         version,
		EngineVersion:        cfg.EngineVersion,
	}
	a := &Agent{cfg: cfg, id: id, cp: cp, probe: probe, handoffWriter: writeHandoff}
	a.engine = newPostgresEngine(cfg, execCommandRunner, execSessionRunner, probe)
	a.hb = newHeartbeater(cp, probe, a.engine, handlers_rds.HeartbeatInterval)
	a.cmd = newCommander(cp, newCommandRegistry(a.engine, newGuestStorage(cfg, execCommandRunner)), cfg.PollWait)
	a.guard = newParamGuard(a.engine, probe, cp)
	return a
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
	return newAgent(cfg, cp, newEngineProbe(cfg, execProbeRunner)), nil
}

func (a *Agent) Run(ctx context.Context) error {
	// Named before the first dial. When register cannot reach the gateway the
	// retry loop is all that survives, and the address it is dialing is what
	// separates a mgmt NIC that came up without an address from a control plane
	// that is genuinely down.
	slog.Info("rds-agent: starting",
		"gatewayURL", a.cfg.GatewayURL, "dbInstanceIdentifier", a.id.DBInstanceIdentifier,
		"agentVersion", a.id.AgentVersion)

	// Registration comes first: its response is the agent's identity.
	if err := a.register(ctx); err != nil {
		return err
	}
	a.hb.id, a.cmd.id, a.guard.id = a.id, a.id, a.id

	// Beating before the bootstrap keeps a stuck boot visible as a live VM with
	// a down engine rather than as silence.
	go a.hb.Run(ctx)

	if err := a.bootstrap(ctx); err != nil {
		return err
	}

	// Directives are only meaningful against a bootstrapped engine.
	go a.cmd.Run(ctx)

	// Started after the handoff, so the window it measures covers the engine's
	// own start rather than the time rds-init spent waiting for this agent.
	go a.guard.Run(ctx)

	<-ctx.Done()
	return nil
}

// Boot-retry bounds. Tight to start, since the usual cause is a control plane
// still creating this instance's record, and capped so an outage does not turn
// a booting fleet into a retry storm.
const (
	retryMin = 1 * time.Second
	retryMax = 30 * time.Second
)

// Never gives up on its own: an agent that stopped trying would leave the
// control plane with no signal at all.
func retry(ctx context.Context, what string, fn func(context.Context) error) error {
	delay := retryMin
	for attempt := 1; ; attempt++ {
		err := fn(ctx)
		if err == nil {
			return nil
		}
		if ctx.Err() != nil {
			return fmt.Errorf("%s: %w", what, ctx.Err())
		}
		slog.Warn("rds-agent: "+what+" failed, retrying", "attempt", attempt, "retryIn", delay, "err", err)

		select {
		case <-ctx.Done():
			return fmt.Errorf("%s: %w", what, ctx.Err())
		case <-time.After(delay):
		}
		delay = min(delay*2, retryMax)
	}
}
