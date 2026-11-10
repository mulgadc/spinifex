// Package ocibsd runs the ocibsd EBS role on one node: it reads the node's
// OCI instance, starts the OCI Block Volume provider and serves it over the
// ebs.provider.v1.* NATS contract with this node's per-node subjects.
package ocibsd

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/nats-io/nats.go"

	natsmsg "github.com/mulgadc/spinifex/spinifex/foundation/messaging/nats"
	"github.com/mulgadc/spinifex/spinifex/foundation/netaddr"
	"github.com/mulgadc/spinifex/spinifex/providers/cloud/oci"
	"github.com/mulgadc/spinifex/spinifex/providers/ebs/natsserve"
	ebsocibsd "github.com/mulgadc/spinifex/spinifex/providers/ebs/ocibsd"
	hostprocess "github.com/mulgadc/spinifex/spinifex/runtime/host/process"
)

// serviceName roots this daemon's PID file: baseDir/ocibsd.pid.
var serviceName = "ocibsd"

// startupTimeout bounds reading the instance and the start-time OCI calls.
const startupTimeout = 2 * time.Minute

// Auth values, as in [nodes.<name>.ebs.oci] auth.
const (
	AuthConfigFile        = "config_file"
	AuthInstancePrincipal = "instance_principal"
)

// Config is the ocibsd service's startup configuration. NodeName scopes the
// PublishVolume and UnpublishVolume subjects to this node, and ClusterID
// names the cluster on every OCI resource.
type Config struct {
	BaseDir    string
	NatsHost   string
	NatsToken  string
	NatsCACert string
	NodeName   string
	// AvailabilityZone is the EC2 zone the node serves.
	AvailabilityZone string

	ClusterID          string
	CompartmentID      string
	AvailabilityDomain string
	Auth               string
	ConfigFile         string
	ConfigProfile      string

	Debug bool
}

// Service runs the ocibsd EBSProvider behind natsserve.
type Service struct {
	Config *Config
}

// New returns the ocibsd service. config must be a *Config.
func New(config any) (*Service, error) {
	cfg, ok := config.(*Config)
	if !ok {
		return nil, errors.New("invalid config type for ocibsd service")
	}
	return &Service{Config: cfg}, nil
}

func (svc *Service) Start() (int, error) {
	if err := hostprocess.WritePidFileTo(svc.Config.BaseDir, serviceName, os.Getpid()); err != nil {
		return 0, fmt.Errorf("write pid file: %w", err)
	}
	if err := launchService(svc.Config, productionDeps()); err != nil {
		slog.Error("Failed to launch service", "err", err)
		return 0, err
	}
	return os.Getpid(), nil
}

// deps are the host and cloud seams the service starts through.
type deps struct {
	connect   func(cfg *Config) (*nats.Conn, error)
	instance  func(ctx context.Context) (oci.InstanceMetadata, error)
	newClient func(cfg *Config) (oci.BlockClient, error)
	initiator ebsocibsd.Initiator
}

func productionDeps() deps {
	return deps{
		connect: func(cfg *Config) (*nats.Conn, error) {
			return natsmsg.ConnectNATSWithRetry(netaddr.DialTarget(cfg.NatsHost), cfg.NatsToken, cfg.NatsCACert)
		},
		instance:  oci.NewMetadataClient().Instance,
		newClient: BlockClientFor,
		initiator: ebsocibsd.NewISCSIAdm(ebsocibsd.ExecRunner{}),
	}
}

var (
	newInstancePrincipalBlockClient = oci.NewInstancePrincipalBlockClient
	newConfigFileBlockClient        = oci.NewConfigFileBlockClient
)

// BlockClientFor builds the OCI client the configured credential selects;
// an unset auth reads a config file, as an oci external pool does.
func BlockClientFor(cfg *Config) (oci.BlockClient, error) {
	switch cfg.Auth {
	case AuthInstancePrincipal:
		return newInstancePrincipalBlockClient()
	case "", AuthConfigFile:
		return newConfigFileBlockClient(cfg.ConfigFile, cfg.ConfigProfile)
	default:
		return nil, fmt.Errorf("ocibsd: auth must be %q or %q, got %q", AuthConfigFile, AuthInstancePrincipal, cfg.Auth)
	}
}

// newProvider reads this node's instance, refuses a node outside the
// cluster's AD, and starts the provider, which logs out stale sessions.
func newProvider(ctx context.Context, cfg *Config, d deps) (*ebsocibsd.Provider, error) {
	if cfg.ClusterID == "" {
		return nil, fmt.Errorf("ocibsd: [nodes.%s.ebs.oci] cluster_id is unset; spx admin init writes it when the provider resolves to ocibsd", cfg.NodeName)
	}
	md, err := d.instance(ctx)
	if err != nil {
		return nil, fmt.Errorf("ocibsd: read this node's OCI instance metadata (ocibsd runs only on an OCI instance): %w", err)
	}
	client, err := d.newClient(cfg)
	if err != nil {
		return nil, fmt.Errorf("ocibsd: build the OCI block client: %w", err)
	}
	p, err := ebsocibsd.New(ebsocibsd.Config{
		ClusterID:          cfg.ClusterID,
		CompartmentID:      cfg.CompartmentID,
		AvailabilityDomain: cfg.AvailabilityDomain,
		AvailabilityZone:   cfg.AvailabilityZone,
		NodeID:             cfg.NodeName,
		InstanceID:         md.ID,
		// The AD check in New compares this with the configured one.
		InstanceAvailabilityDomain: md.AvailabilityDomain,
		Client:                     client,
		Initiator:                  d.initiator,
	})
	if err != nil {
		return nil, err
	}
	if err := p.Start(ctx); err != nil {
		return nil, err
	}
	slog.InfoContext(ctx, "ocibsd: started", "node", cfg.NodeName, "instance", md.ID, "cluster", cfg.ClusterID)
	return p, nil
}

// serve starts the provider and serves it over nc until stop is called.
func serve(ctx context.Context, nc *nats.Conn, cfg *Config, d deps) (stop func(), err error) {
	startCtx, cancel := context.WithTimeout(ctx, startupTimeout)
	defer cancel()
	p, err := newProvider(startCtx, cfg, d)
	if err != nil {
		return nil, err
	}
	stop, err = natsserve.Serve(ctx, nc, p, natsserve.Options{NodeID: cfg.NodeName})
	if err != nil {
		return nil, fmt.Errorf("serve ebs.provider.v1: %w", err)
	}
	return stop, nil
}

// launchService connects to NATS and serves until SIGINT or SIGTERM.
func launchService(cfg *Config, d deps) error {
	nc, err := d.connect(cfg)
	if err != nil {
		return fmt.Errorf("connect to NATS: %w", err)
	}
	defer nc.Close()

	stop, err := serve(context.Background(), nc, cfg, d)
	if err != nil {
		return err
	}
	slog.Info("ocibsd: waiting for EBS provider events", "node", cfg.NodeName)

	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)
	<-sigChan
	slog.Info("ocibsd: shutting down gracefully...")
	stop()
	return nil
}
