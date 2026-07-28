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

// version is the agent build version, reported on registration.
// Overridable via -ldflags "-X main.version=...".
var version = "dev"

// identity is what the agent says about itself on every call. The gateway
// resolves the authoritative DB instance from the credentials the request is
// signed with, so DBInstanceIdentifier is an assertion to be checked rather than
// a claim to be trusted — it is empty until configured or until the register
// response supplies it.
type identity struct {
	DBInstanceIdentifier string
	AgentVersion         string
	EngineVersion        string
}

// controlPlane is the agent's channel to the AWS gateway. Every method is a
// SigV4-signed Query request, so the NATS bus stays host-internal. Tests inject
// a fake; production uses gatewayControlPlane.
type controlPlane interface {
	// Register records this VM's boot. It is idempotent, and its response is
	// what tells the agent which DB instance it backs and how often to beat.
	Register(ctx context.Context, id identity) (*handlers_rds.RegisterDBInstanceOutput, error)
	// SubmitState reports engine health, folding liveness into the same call so
	// a healthy instance costs one round trip per tick.
	SubmitState(ctx context.Context, id identity, health handlers_rds.EngineHealth, message string) (*handlers_rds.SubmitDBStateChangeOutput, error)
	// GetBootstrapConfig fetches the material rds-init needs. The first call of
	// an instance's life returns Mode=initialize with the master password; every
	// later call returns Mode=attach without it.
	GetBootstrapConfig(ctx context.Context, id identity) (*handlers_rds.GetDBBootstrapConfigOutput, error)
	// PollCommands delivers replies for commands executed since the last poll
	// and long-polls for the next directive, returning no commands when the
	// window closes idle.
	PollCommands(ctx context.Context, id identity, replies []handlers_rds.CommandReply, wait time.Duration) ([]handlers_rds.Command, error)
}

// callTimeout bounds a register, heartbeat or bootstrap request. The long poll
// sets its own deadline from the wait it asked the gateway to hold.
const callTimeout = 15 * time.Second

// pollSlack is how much longer than the requested wait the poll's own deadline
// runs, so a gateway returning at the end of its window is not cut off by the
// client a moment before its answer arrives.
const pollSlack = 10 * time.Second

// gatewayControlPlane implements controlPlane over the SigV4 Query client.
type gatewayControlPlane struct {
	client *rdsgw.Client
}

var _ controlPlane = (*gatewayControlPlane)(nil)

// newGatewayControlPlane builds the client signing with the instance-role
// credentials the SDK chain resolves from IMDS, against the pinned gateway CA.
func newGatewayControlPlane(cfg config) (*gatewayControlPlane, error) {
	signer, err := gwsign.NewIMDS(context.Background(), cfg.Region)
	if err != nil {
		return nil, fmt.Errorf("build IMDS signer: %w", err)
	}
	// The client timeout is the ceiling for the longest call, the long poll;
	// shorter calls carry their own context deadline.
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

// pollDBCommandsOutput mirrors the gateway's PollDBCommands result. The agent
// declares its own rather than importing the gateway package, so an in-guest
// binary does not link the server side of the API it calls.
type pollDBCommandsOutput struct {
	Commands []handlers_rds.Command `xml:"Commands>member"`
}

func (g *gatewayControlPlane) PollCommands(ctx context.Context, id identity, replies []handlers_rds.CommandReply, wait time.Duration) ([]handlers_rds.Command, error) {
	params := url.Values{"WaitTimeSeconds": {strconv.FormatInt(int64(wait.Seconds()), 10)}}
	setIfPresent(params, "DBInstanceIdentifier", id.DBInstanceIdentifier)
	// Replies ride the poll rather than using their own action, so a reply and
	// the request for the next command are one round trip.
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

// setIfPresent adds a query parameter only when it has a value, so an unset
// optional field is absent from the request rather than sent empty — which is
// what lets the gateway tell "no assertion" from "asserted as blank".
func setIfPresent(params url.Values, key, value string) {
	if value != "" {
		params.Set(key, value)
	}
}

// Agent wires the rds-agent's runtime seams: the gateway control plane, the
// engine health probe, the bootstrap handoff writer and the command channel. It
// registers the VM, delivers the bootstrap config rds-init is waiting on, then
// heartbeats engine health and polls for directives until shutdown.
type Agent struct {
	cfg   config
	id    identity
	cp    controlPlane
	probe *engineProbe

	hb  *heartbeater
	cmd *commander
}

// newAgent assembles an Agent from already-built seams. Tests use this directly
// with fakes; New builds the production control plane and delegates here.
func newAgent(cfg config, cp controlPlane, probe *engineProbe) *Agent {
	id := identity{
		DBInstanceIdentifier: cfg.DBInstanceIdentifier,
		AgentVersion:         version,
		EngineVersion:        cfg.EngineVersion,
	}
	a := &Agent{cfg: cfg, id: id, cp: cp, probe: probe}
	a.hb = newHeartbeater(cp, probe, handlers_rds.HeartbeatInterval)
	a.cmd = newCommander(cp, newCommandRegistry(), cfg.PollWait)
	return a
}

// New builds an Agent from config, including the SigV4 gateway client. It does
// not wait for IMDS: the register loop rides out a datapath that is still coming
// up, and failing here would only make that wait less visible.
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

// Run drives the agent's whole life: register, deliver the bootstrap handoff,
// then heartbeat and poll until ctx is cancelled.
//
// The order is what the boot depends on. Registration comes first because its
// response is the agent's identity; the heartbeat starts next so a bootstrap
// that cannot complete is still visible to the control plane as a live VM with
// a down engine, rather than as silence; the command loop starts last, since a
// directive is only meaningful against an engine that has been bootstrapped.
func (a *Agent) Run(ctx context.Context) error {
	if err := a.register(ctx); err != nil {
		return err
	}
	a.hb.id, a.cmd.id = a.id, a.id

	go a.hb.Run(ctx)

	if err := a.bootstrap(ctx); err != nil {
		return err
	}

	go a.cmd.Run(ctx)

	<-ctx.Done()
	return nil
}

// Boot-retry bounds. Both boot-critical calls start tight, because the usual
// cause of a failure is a control plane that has not finished creating this
// instance's record yet, and cap so an outage does not turn a booting fleet into
// a retry storm.
const (
	retryMin = 1 * time.Second
	retryMax = 30 * time.Second
)

// retry runs fn until it succeeds or ctx ends, doubling the gap between
// attempts. It never gives up on its own: an agent that stopped trying would
// leave the control plane with no signal at all, where one that keeps trying
// recovers by itself the moment the gateway comes back.
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
