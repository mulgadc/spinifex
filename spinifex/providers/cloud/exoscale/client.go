// Package exoscale is the narrow slice of the Exoscale API the external
// allocator needs, driven through the exo CLI rather than the Go SDK.
//
// The CLI keeps a heavy dependency out of the build while this is a proof of
// concept, and it means every call Spinifex makes is one an operator can repeat
// by hand with the same config file. Nothing outside this package knows a
// process is being run, so moving to the SDK later changes only this file.
package exoscale

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"os"
	"os/exec"
	"strings"
	"time"
)

// DefaultBinary is where the exo release package installs the CLI.
const DefaultBinary = "/usr/bin/exo"

// DefaultConfigFile holds the node's scoped API key, in the exo CLI's own
// format, readable by the daemon's service user only.
const DefaultConfigFile = "/etc/spinifex/exoscale/exoscale.toml"

// DefaultCallTimeout bounds one CLI invocation. Allocation sits inside the AWS
// client's 60s read timeout and makes two calls, so each gets well under half.
const DefaultCallTimeout = 25 * time.Second

var (
	// ErrNotFound means the EIP or instance does not exist in the zone.
	ErrNotFound = errors.New("exoscale: resource not found")
	// ErrQuotaExceeded means the organisation's EIP quota is used up.
	ErrQuotaExceeded = errors.New("exoscale: quota exceeded")
)

// ElasticIP is one Exoscale Elastic IP as the allocator sees it.
type ElasticIP struct {
	ID          string
	Address     netip.Addr
	Description string
	Zone        string
}

// Client is the surface the allocator uses. Expressed in this package's types
// so the fake in fake.go is a map, not a mock of a process.
type Client interface {
	// CreateElasticIP creates an unattached manual IPv4 EIP.
	CreateElasticIP(ctx context.Context, description string) (ElasticIP, error)
	// AttachElasticIP attaches eipID to instanceID.
	AttachElasticIP(ctx context.Context, instanceID, eipID string) error
	// DetachElasticIP detaches eipID from instanceID, keeping the address.
	DetachElasticIP(ctx context.Context, instanceID, eipID string) error
	// DeleteElasticIP releases the address back to Exoscale.
	DeleteElasticIP(ctx context.Context, eipID string) error
	// ListElasticIPs returns every EIP in the zone, ours or not.
	ListElasticIPs(ctx context.Context) ([]ElasticIP, error)
	// Version reports the CLI version, proving the binary runs at all.
	Version(ctx context.Context) (string, error)
}

// runFunc executes the CLI. Swapped in tests so no process is started.
type runFunc func(ctx context.Context, binary string, args []string) (stdout, stderr []byte, err error)

// CLIConfig selects the binary, credentials and zone.
type CLIConfig struct {
	Binary      string
	ConfigFile  string
	Account     string
	Zone        string
	CallTimeout time.Duration
}

// cliClient implements Client by running exo with JSON output.
type cliClient struct {
	cfg CLIConfig
	run runFunc
}

var _ Client = (*cliClient)(nil)

// NewCLIClient builds a client over the exo binary. It does not run anything;
// call Version to prove the binary and config are usable.
func NewCLIClient(cfg CLIConfig) (Client, error) {
	if cfg.Zone == "" {
		return nil, errors.New("exoscale: zone is required")
	}
	if cfg.Binary == "" {
		cfg.Binary = DefaultBinary
	}
	if cfg.ConfigFile == "" {
		cfg.ConfigFile = DefaultConfigFile
	}
	if cfg.CallTimeout <= 0 {
		cfg.CallTimeout = DefaultCallTimeout
	}
	return &cliClient{cfg: cfg, run: execRun}, nil
}

// eipJSON is the shape of `exo compute elastic-ip create|show|list -O json`.
type eipJSON struct {
	ID          string `json:"id"`
	IPAddress   string `json:"ip_address"`
	Description string `json:"description"`
	Zone        string `json:"zone"`
}

func (e eipJSON) toElasticIP() (ElasticIP, error) {
	if e.ID == "" {
		return ElasticIP{}, errors.New("exoscale: elastic IP with no id in exo output")
	}
	addr, err := netip.ParseAddr(e.IPAddress)
	if err != nil {
		return ElasticIP{}, fmt.Errorf("exoscale: elastic IP %s has unparseable address %q: %w", e.ID, e.IPAddress, err)
	}
	return ElasticIP{ID: e.ID, Address: addr, Description: e.Description, Zone: e.Zone}, nil
}

func (c *cliClient) CreateElasticIP(ctx context.Context, description string) (ElasticIP, error) {
	out, err := c.exo(ctx, "compute", "elastic-ip", "create", "--description", description, "-z", c.cfg.Zone)
	if err != nil {
		return ElasticIP{}, err
	}
	var e eipJSON
	if err := json.Unmarshal(out, &e); err != nil {
		return ElasticIP{}, fmt.Errorf("exoscale: decode created elastic IP: %w", err)
	}
	return e.toElasticIP()
}

func (c *cliClient) AttachElasticIP(ctx context.Context, instanceID, eipID string) error {
	_, err := c.exo(ctx, "compute", "instance", "elastic-ip", "attach", instanceID, eipID, "-z", c.cfg.Zone)
	return err
}

func (c *cliClient) DetachElasticIP(ctx context.Context, instanceID, eipID string) error {
	_, err := c.exo(ctx, "compute", "instance", "elastic-ip", "detach", instanceID, eipID, "-z", c.cfg.Zone)
	return err
}

func (c *cliClient) DeleteElasticIP(ctx context.Context, eipID string) error {
	_, err := c.exo(ctx, "compute", "elastic-ip", "delete", "--force", eipID, "-z", c.cfg.Zone)
	return err
}

func (c *cliClient) ListElasticIPs(ctx context.Context) ([]ElasticIP, error) {
	out, err := c.exo(ctx, "compute", "elastic-ip", "list", "-z", c.cfg.Zone)
	if err != nil {
		return nil, err
	}
	var raw []eipJSON
	if err := json.Unmarshal(out, &raw); err != nil {
		return nil, fmt.Errorf("exoscale: decode elastic IP list: %w", err)
	}
	eips := make([]ElasticIP, 0, len(raw))
	for _, e := range raw {
		// The list spans zones on some CLI versions; only ours is ours to judge.
		if e.Zone != "" && e.Zone != c.cfg.Zone {
			continue
		}
		eip, err := e.toElasticIP()
		if err != nil {
			return nil, err
		}
		eips = append(eips, eip)
	}
	return eips, nil
}

func (c *cliClient) Version(ctx context.Context) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, c.cfg.CallTimeout)
	defer cancel()
	stdout, stderr, err := c.run(ctx, c.cfg.Binary, []string{"version"})
	if err != nil {
		return "", classify(err, stderr)
	}
	return strings.TrimSpace(string(stdout)), nil
}

// exo runs one command with JSON output against the configured account.
func (c *cliClient) exo(ctx context.Context, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, c.cfg.CallTimeout)
	defer cancel()
	full := []string{"--config", c.cfg.ConfigFile, "--output-format", "json", "--quiet"}
	if c.cfg.Account != "" {
		full = append(full, "--use-account", c.cfg.Account)
	}
	full = append(full, args...)
	stdout, stderr, err := c.run(ctx, c.cfg.Binary, full)
	if err != nil {
		return nil, fmt.Errorf("exo %s: %w", strings.Join(args, " "), classify(err, stderr))
	}
	return stdout, nil
}

// classify maps the CLI's stderr onto the sentinels the allocator acts on. The
// texts are the ones exo 1.101 prints; anything else keeps its own message.
func classify(runErr error, stderr []byte) error {
	msg := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(string(stderr)), "error:"))
	switch {
	case strings.Contains(msg, "resource not found"):
		return fmt.Errorf("%w: %s", ErrNotFound, msg)
	case strings.Contains(msg, "has been exceeded"):
		return fmt.Errorf("%w: %s", ErrQuotaExceeded, msg)
	case msg != "":
		return fmt.Errorf("%s: %w", msg, runErr)
	default:
		return runErr
	}
}

// credentialEnv are the variables exo would read in preference to --config. A
// daemon started with them in its environment would act as a different key
// from the one the operator provisioned, so they are dropped.
var credentialEnv = []string{"EXOSCALE_API_KEY=", "EXOSCALE_API_SECRET=", "EXOSCALE_ACCOUNT=", "EXOSCALE_CONFIG="}

func execRun(ctx context.Context, binary string, args []string) ([]byte, []byte, error) {
	cmd := exec.CommandContext(ctx, binary, args...)
	cmd.Env = scrubEnv(os.Environ())
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	return stdout.Bytes(), stderr.Bytes(), err
}

func scrubEnv(env []string) []string {
	out := env[:0:0]
	for _, kv := range env {
		drop := false
		for _, prefix := range credentialEnv {
			if strings.HasPrefix(kv, prefix) {
				drop = true
				break
			}
		}
		if !drop {
			out = append(out, kv)
		}
	}
	return out
}
