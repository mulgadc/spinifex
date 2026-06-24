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

	"github.com/nats-io/nats.go"

	"github.com/mulgadc/spinifex/cmd/ecs-agent/credentials"
	ctrruntime "github.com/mulgadc/spinifex/cmd/ecs-agent/runtime"
	"github.com/mulgadc/spinifex/spinifex/handlers/ecs/bus"
)

// version is the agent build version, reported in the register message.
// Overridable via -ldflags "-X main.version=...".
var version = "dev"

// Agent wires the ecs-agent's runtime seams: a NATS publisher for the Layer-2
// bus, a container runtime, and an ECR resolver. It registers the host as a
// container instance at boot and heartbeats while alive. Task assignment and
// container lifecycle land in Sprint 4e.
type Agent struct {
	cfg      config
	id       identity
	puller   ctrruntime.ImagePuller
	resolver ctrruntime.Resolver

	reg *registrar
	hb  *heartbeater

	nc      *nats.Conn
	closers []func() error
}

// newAgent assembles an Agent from already-built seams. Tests use this directly
// with fakes; New builds the production seams and delegates here.
func newAgent(cfg config, id identity, pub publisher, puller ctrruntime.ImagePuller, resolver ctrruntime.Resolver) *Agent {
	return &Agent{
		cfg:      cfg,
		id:       id,
		puller:   puller,
		resolver: resolver,
		reg:      newRegistrar(pub, id),
		hb:       newHeartbeater(pub, id, cfg.Heartbeat, nil),
	}
}

// New builds an Agent from config: it resolves the host identity from IMDS,
// connects to NATS, builds the ECR resolver and (best-effort) the containerd
// runtime. A containerd connect failure is logged, not fatal — registration and
// heartbeat still run so the instance is visible while the runtime recovers.
// The ECR gateway client is built lazily on first image pull (not here), so a
// missing or malformed gateway CA does not stop the agent from registering.
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
	if p, perr := ctrruntime.New(cfg.ContainerdSocket); perr != nil {
		slog.Warn("ecs-agent: containerd unavailable at boot, image pulls disabled", "err", perr)
	} else {
		puller = p
	}

	nc, err := nats.Connect(cfg.NATSURL, nats.Name("ecs-agent/"+id.InstanceID))
	if err != nil {
		return nil, fmt.Errorf("connect nats %s: %w", cfg.NATSURL, err)
	}

	a := newAgent(cfg, id, nc, puller, resolver)
	a.nc = nc
	if puller != nil {
		a.closers = append(a.closers, puller.Close)
	}
	return a, nil
}

// Run registers the instance, starts the heartbeat loop, and blocks until ctx is
// cancelled, then tears down.
func (a *Agent) Run(ctx context.Context) error {
	if err := a.reg.Register(); err != nil {
		slog.Error("ecs-agent: initial registration failed", "err", err)
	} else {
		slog.Info("ecs-agent: registered",
			"cluster", a.id.ClusterName, "instance", a.id.InstanceID,
			"subject", bus.RegisterSubject(a.id.AccountID, a.id.ClusterName, a.id.InstanceID))
	}

	go a.hb.Run(ctx)

	<-ctx.Done()
	return a.Stop()
}

// Stop drains NATS and closes the runtime. Safe to call once.
func (a *Agent) Stop() error {
	var firstErr error
	for _, c := range a.closers {
		if err := c(); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	if a.nc != nil {
		_ = a.nc.Drain()
		a.nc.Close()
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
