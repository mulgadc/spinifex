package main

import (
	"bufio"
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	goruntime "runtime"
	"strconv"
	"strings"
	"time"

	"github.com/mulgadc/spinifex/cmd/ecs-agent/credentials"
	ctrruntime "github.com/mulgadc/spinifex/cmd/ecs-agent/runtime"
	"github.com/mulgadc/spinifex/spinifex/handlers/ecs/bus"
)

// version is the agent build version, reported in the register message.
// Overridable via -ldflags "-X main.version=...".
var version = "dev"

// Agent wires the ecs-agent's runtime seams: a gateway control-plane client, a
// container runtime, and an ECR resolver. It registers the host as a container
// instance at boot, heartbeats (re-registers) while alive, polls the gateway for
// task assignments, and runs them through containerd, reporting state back over
// the gateway. It never connects to NATS — the bus stays host-internal.
type Agent struct {
	cfg      config
	id       identity
	cp       controlPlane
	puller   ctrruntime.ImagePuller
	runner   ctrruntime.Runner
	resolver ctrruntime.Resolver

	reg *registrar
	hb  *heartbeater

	// netns builds awsvpc task network namespaces (nil-safe; bridge/host skip it).
	netns *taskNetns

	// cred serves task IAM role credentials at 169.254.170.2 (nil in unit tests).
	cred *credEndpoint

	closers []func() error
}

// newAgent assembles an Agent from already-built seams. Tests use this directly
// with fakes; New builds the production seams and delegates here. runner may be
// nil when containerd is unavailable; the assign path then reports the task
// STOPPED rather than crashing.
func newAgent(cfg config, id identity, cp controlPlane, puller ctrruntime.ImagePuller, runner ctrruntime.Runner, resolver ctrruntime.Resolver) *Agent {
	return &Agent{
		cfg:      cfg,
		id:       id,
		cp:       cp,
		puller:   puller,
		runner:   runner,
		resolver: resolver,
		reg:      newRegistrar(cp, id),
		hb:       newHeartbeater(cp, id, cfg.Heartbeat),
		netns:    newTaskNetns(execNetRunner{}),
	}
}

// New builds an Agent from config: it resolves the host identity from IMDS,
// builds the gateway control-plane client, the ECR resolver and (best-effort)
// the containerd runtime. A containerd connect failure is logged, not fatal —
// registration and heartbeat still run so the instance is visible while the
// runtime recovers. The ECR gateway client is built lazily on first image pull
// (not here), so a missing or malformed gateway CA does not stop registration.
func New(cfg config) (*Agent, error) {
	imdsClient := &http.Client{Timeout: 5 * time.Second}

	meta, err := fetchInstanceMetadata(imdsClient, cfg.IMDSBase)
	if err != nil {
		return nil, fmt.Errorf("instance metadata: %w", err)
	}
	host, _ := os.Hostname()
	id := identity{
		AccountID:    meta.AccountID,
		ClusterName:  cfg.ClusterName,
		InstanceID:   meta.InstanceID,
		AZ:           meta.AZ,
		Hostname:     host,
		Capacity:     detectCapacity(),
		AgentVersion: version,
	}

	creds := credentials.NewIMDSProvider(imdsClient, cfg.IMDSBase)
	resolver := newLazyECRResolver(creds, cfg.Region, cfg.GatewayURL, cfg.GatewayCA)

	var puller ctrruntime.ImagePuller
	var runner ctrruntime.Runner
	if rt, perr := ctrruntime.New(cfg.ContainerdSocket); perr != nil {
		slog.Warn("ecs-agent: containerd unavailable at boot, image pulls disabled", "err", perr)
	} else {
		puller = rt
		runner = rt
	}

	cp, err := newGatewayControlPlane(cfg)
	if err != nil {
		return nil, fmt.Errorf("build gateway control-plane: %w", err)
	}

	a := newAgent(cfg, id, cp, puller, runner, resolver)
	a.cred = newCredEndpoint(creds, cfg.Region, cfg.GatewayURL, cfg.GatewayCA,
		cfg.CredEndpointIP, cfg.CredEndpointPort, execNetRunner{})
	if puller != nil {
		a.closers = append(a.closers, puller.Close)
	}
	return a, nil
}

// Run registers the instance, starts the heartbeat and assignment-poll loops,
// and blocks until ctx is cancelled, then tears down.
func (a *Agent) Run(ctx context.Context) error {
	if err := a.reg.Register(); err != nil {
		slog.Error("ecs-agent: initial registration failed", "err", err)
	} else {
		slog.Info("ecs-agent: registered", "cluster", a.id.ClusterName, "instance", a.id.InstanceID)
	}

	if a.cred != nil {
		if err := a.cred.Start(); err != nil {
			slog.Error("ecs-agent: credential endpoint start failed", "err", err)
		}
	}

	go a.hb.Run(ctx)
	go a.pollAssignments(ctx)

	<-ctx.Done()
	return a.Stop()
}

// Stop closes the runtime. Safe to call once.
func (a *Agent) Stop() error {
	var firstErr error
	if a.cred != nil {
		_ = a.cred.Stop()
	}
	for _, c := range a.closers {
		if err := c(); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// detectCapacity reports the host's total CPU units (1 vCPU = 1024) and memory.
func detectCapacity() bus.InstanceCapacity {
	return bus.InstanceCapacity{
		CPU:       goruntime.NumCPU() * 1024,
		MemoryMiB: memTotalMiB(),
	}
}

// memTotalMiB reads MemTotal from /proc/meminfo, returning 0 if unavailable.
func memTotalMiB() int {
	f, err := os.Open("/proc/meminfo")
	if err != nil {
		return 0
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "MemTotal:") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			return 0
		}
		kb, err := strconv.Atoi(fields[1])
		if err != nil {
			return 0
		}
		return kb / 1024
	}
	return 0
}
