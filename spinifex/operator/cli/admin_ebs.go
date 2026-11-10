package cli

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	toml "github.com/pelletier/go-toml/v2"

	"github.com/mulgadc/spinifex/spinifex/bootstrap/config"
	"github.com/mulgadc/spinifex/spinifex/operator/admin"
	"github.com/mulgadc/spinifex/spinifex/providers/cloud/oci"
	ebsocibsd "github.com/mulgadc/spinifex/spinifex/providers/ebs/ocibsd"
	"github.com/mulgadc/spinifex/spinifex/runtime/formation"
	ocibsdrole "github.com/mulgadc/spinifex/spinifex/runtime/roles/ocibsd"
)

// ebsProviderEnv names the provider for an install with no existing config,
// in place of detection.
const ebsProviderEnv = "SPINIFEX_EBS_PROVIDER"

const (
	// imdsDetectTimeout bounds the metadata read detection makes, so an
	// install off OCI pays at most this for it.
	imdsDetectTimeout = time.Second
	// credentialCheckTimeout bounds the OCI read the credential check makes.
	credentialCheckTimeout = 30 * time.Second
)

// Where a node's provider came from, printed by init and join so a run can
// tell a detected provider from a configured one.
const (
	ebsFromConfig    = "from the existing config"
	ebsFromEnv       = "from " + ebsProviderEnv
	ebsFromDetection = "detected"
	ebsFromFormation = "from formation"
)

// nodeEBS is one node's EBS choice: the cluster-wide part formation carries
// to joining nodes, and this node's own OCI credential.
type nodeEBS struct {
	formation.EBSConfig

	Auth          string
	ConfigFile    string
	ConfigProfile string
	// Source is one of the ebsFrom* values; Detail says what detection saw.
	Source string
	Detail string
}

// describe is the line init and join print for the choice.
func (e nodeEBS) describe() string {
	provider := (config.EBSConfig{Provider: e.Provider}).ResolvedProvider()
	if e.Detail != "" {
		return fmt.Sprintf("EBS provider: %s (%s: %s)", provider, e.Source, e.Detail)
	}
	return fmt.Sprintf("EBS provider: %s (%s)", provider, e.Source)
}

// apply copies the choice into the settings spinifex.toml is rendered from.
func (e nodeEBS) apply(s *admin.ConfigSettings) {
	s.EBSProvider = e.Provider
	s.OCICompartmentID = e.OCICompartmentID
	s.OCIAvailabilityDomain = e.OCIAvailabilityDomain
	s.OCIClusterID = e.ClusterID
	s.OCIAuth = e.Auth
	s.OCIConfigFile = e.ConfigFile
	s.OCIConfigProfile = e.ConfigProfile
}

// ebsResolver holds the host and cloud seams provider resolution reads.
type ebsResolver struct {
	getenv   func(string) string
	instance func(ctx context.Context) (oci.InstanceMetadata, error)
	// newClient builds the OCI client the credential check reads through.
	newClient func(auth, configFile, configProfile string) (oci.BlockClient, error)
}

func productionEBSResolver() ebsResolver {
	md := &oci.MetadataClient{BaseURL: oci.IMDSBaseURL, HTTP: &http.Client{Timeout: imdsDetectTimeout}}
	return ebsResolver{
		getenv:   os.Getenv,
		instance: md.Instance,
		newClient: func(auth, configFile, configProfile string) (oci.BlockClient, error) {
			return ocibsdrole.BlockClientFor(&ocibsdrole.Config{Auth: auth, ConfigFile: configFile, ConfigProfile: configProfile})
		},
	}
}

// existingEBS is a node's [nodes.<node>.ebs] in a spinifex.toml already on
// disk, kept as written: an empty Provider is the default, viperblockd.
type existingEBS struct {
	Provider string `toml:"provider"`
	OCI      struct {
		CompartmentID      string `toml:"compartment_id"`
		AvailabilityDomain string `toml:"availability_domain"`
		ClusterID          string `toml:"cluster_id"`
		Auth               string `toml:"auth"`
		ConfigFile         string `toml:"config_file"`
		ConfigProfile      string `toml:"config_profile"`
	} `toml:"oci"`
}

// readExistingEBS returns node's EBS table from tomlPath, nil when there is
// no file or it has no [nodes.<node>]. A file that cannot be read is an
// error: the provider it names must never be guessed.
func readExistingEBS(tomlPath, node string) (*existingEBS, error) {
	raw, err := os.ReadFile(tomlPath)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read the existing config %s: %w", tomlPath, err)
	}
	var cfg struct {
		Nodes map[string]struct {
			EBS existingEBS `toml:"ebs"`
		} `toml:"nodes"`
	}
	if err := toml.Unmarshal(raw, &cfg); err != nil {
		return nil, fmt.Errorf("parse the existing config %s, whose EBS provider this node keeps: %w", tomlPath, err)
	}
	n, ok := cfg.Nodes[node]
	if !ok {
		return nil, nil
	}
	return &n.EBS, nil
}

// isOCIVM reports whether md is an OCI virtual machine; bare-metal shapes
// stay on viperblockd until a bare-metal iSCSI run is proven.
func isOCIVM(md oci.InstanceMetadata) bool {
	return md.Shape != "" && !strings.HasPrefix(md.Shape, "BM.")
}

// detect reads the OCI instance metadata within imdsDetectTimeout; ok is
// false when nothing answers, which is every host off OCI.
func (r ebsResolver) detect(ctx context.Context) (oci.InstanceMetadata, bool) {
	ctx, cancel := context.WithTimeout(ctx, imdsDetectTimeout)
	defer cancel()
	md, err := r.instance(ctx)
	if err != nil {
		return oci.InstanceMetadata{}, false
	}
	return md, true
}

// resolveProvider picks init's provider: an existing node config's, then
// ebsProviderEnv, then detection. md is set when detection read it.
func (r ebsResolver) resolveProvider(ctx context.Context, existing *existingEBS) (nodeEBS, *oci.InstanceMetadata) {
	switch {
	case existing != nil:
		return nodeEBS{
			EBSConfig: formation.EBSConfig{Provider: (config.EBSConfig{Provider: existing.Provider}).ResolvedProvider()},
			Source:    ebsFromConfig,
		}, nil
	case r.getenv(ebsProviderEnv) != "":
		return nodeEBS{EBSConfig: formation.EBSConfig{Provider: r.getenv(ebsProviderEnv)}, Source: ebsFromEnv}, nil
	}
	md, ok := r.detect(ctx)
	switch {
	case !ok:
		return nodeEBS{EBSConfig: formation.EBSConfig{Provider: config.EBSProviderViperblockd}, Source: ebsFromDetection, Detail: "no OCI instance metadata"}, nil
	case !isOCIVM(md):
		return nodeEBS{EBSConfig: formation.EBSConfig{Provider: config.EBSProviderViperblockd}, Source: ebsFromDetection, Detail: "OCI bare metal " + md.Shape}, nil
	}
	return nodeEBS{EBSConfig: formation.EBSConfig{Provider: config.EBSProviderOCIBSD}, Source: ebsFromDetection, Detail: "OCI virtual machine " + md.Shape}, &md
}

// fillOCIBSD completes an ocibsd choice: the existing config's values win,
// then the instance metadata, and auth defaults to instance_principal.
// md is read here when the caller has none and a value is missing.
func (r ebsResolver) fillOCIBSD(ctx context.Context, e *nodeEBS, existing *existingEBS, md *oci.InstanceMetadata) error {
	var prior existingEBS
	if existing != nil {
		prior = *existing
	}
	if e.OCICompartmentID == "" {
		e.OCICompartmentID = prior.OCI.CompartmentID
	}
	if e.OCIAvailabilityDomain == "" {
		e.OCIAvailabilityDomain = prior.OCI.AvailabilityDomain
	}
	if e.OCICompartmentID == "" || e.OCIAvailabilityDomain == "" {
		if md == nil {
			got, ok := r.detect(ctx)
			if !ok {
				return fmt.Errorf("EBS provider %s (%s) needs this node's OCI instance metadata for its compartment and availability domain, and none answered", config.EBSProviderOCIBSD, e.Source)
			}
			md = &got
		}
		if e.OCICompartmentID == "" {
			e.OCICompartmentID = md.CompartmentID
		}
		if e.OCIAvailabilityDomain == "" {
			e.OCIAvailabilityDomain = md.AvailabilityDomain
		}
	}
	e.Auth, e.ConfigFile, e.ConfigProfile = prior.OCI.Auth, prior.OCI.ConfigFile, prior.OCI.ConfigProfile
	if e.Auth == "" && e.ConfigFile == "" && e.ConfigProfile == "" {
		e.Auth = ocibsdrole.AuthInstancePrincipal
	}
	if e.ClusterID == "" {
		e.ClusterID = prior.OCI.ClusterID
	}
	if e.ClusterID == "" {
		id, err := generateClusterID()
		if err != nil {
			return err
		}
		e.ClusterID = id
	}
	return nil
}

// checkCredential makes the read ocibsd makes at start with this node's
// credential, and never falls back to another provider when it is refused.
func (r ebsResolver) checkCredential(ctx context.Context, e nodeEBS) error {
	client, err := r.newClient(e.Auth, e.ConfigFile, e.ConfigProfile)
	if err != nil {
		return fmt.Errorf("ocibsd: build the OCI client for auth %q: %w", e.Auth, err)
	}
	ctx, cancel := context.WithTimeout(ctx, credentialCheckTimeout)
	defer cancel()
	return ebsocibsd.CheckCredential(ctx, client, e.OCICompartmentID, e.OCIAvailabilityDomain, e.ClusterID)
}

// resolveInitEBS resolves init's provider once, for every node (ADR-0018:S3),
// and for ocibsd completes its table and checks the credential.
func resolveInitEBS(ctx context.Context, tomlPath, node string, r ebsResolver) (nodeEBS, error) {
	existing, err := readExistingEBS(tomlPath, node)
	if err != nil {
		return nodeEBS{}, err
	}
	e, md := r.resolveProvider(ctx, existing)
	if !config.SupportedEBSProvider(e.Provider) {
		return nodeEBS{}, fmt.Errorf("EBS provider %q (%s) is unsupported; use %q, %q or %q", e.Provider, e.Source, config.EBSProviderViperblockd, config.EBSProviderQEMUNBD, config.EBSProviderOCIBSD)
	}
	fmt.Println(e.describe())
	if e.Provider != config.EBSProviderOCIBSD {
		return e, nil
	}
	if err := r.fillOCIBSD(ctx, &e, existing, md); err != nil {
		return nodeEBS{}, err
	}
	if err := r.checkCredential(ctx, e); err != nil {
		return nodeEBS{}, err
	}
	return e, nil
}

// resolveJoinEBS takes the provider from formation; for ocibsd this node's
// own credential comes from its existing config, else instance_principal,
// and is checked before its config is written.
func resolveJoinEBS(ctx context.Context, tomlPath, node string, nc *formation.NetworkConfig, r ebsResolver) (nodeEBS, error) {
	e := nodeEBS{Source: ebsFromFormation}
	if nc != nil && nc.EBS != nil {
		e.EBSConfig = *nc.EBS
	}
	fmt.Println(e.describe())
	if e.Provider != config.EBSProviderOCIBSD {
		return e, nil
	}
	if e.OCICompartmentID == "" || e.OCIAvailabilityDomain == "" || e.ClusterID == "" {
		return nodeEBS{}, errors.New("formation named ocibsd without its compartment, availability domain and cluster ID")
	}
	existing, err := readExistingEBS(tomlPath, node)
	if err != nil {
		return nodeEBS{}, err
	}
	if err := r.fillOCIBSD(ctx, &e, existing, nil); err != nil {
		return nodeEBS{}, err
	}
	if err := r.checkCredential(ctx, e); err != nil {
		return nodeEBS{}, err
	}
	return e, nil
}

// formationEBS is the EBS object sent to joining nodes: nil for the default
// provider, so a viperblock cluster's formation payload has no ebs object.
func formationEBS(c formation.EBSConfig) *formation.EBSConfig {
	if (config.EBSConfig{Provider: c.Provider}).ResolvedProvider() == config.EBSProviderViperblockd {
		return nil
	}
	return &c
}

// generateClusterID returns a random 128-bit value in hex.
func generateClusterID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("generate the cluster ID: %w", err)
	}
	return hex.EncodeToString(b[:]), nil
}

// ocibsdStageDir is where setup.sh stages the tarball's ocibsd/ files on an
// OCI virtual machine.
const ocibsdStageDir = "/usr/local/share/spinifex/ocibsd"

// ocibsdFiles maps each staged file to where activation installs it.
var ocibsdFiles = []struct{ staged, installed string }{
	{"spinifex-ocibsd.service", "etc/systemd/system/spinifex-ocibsd.service"},
	{"99-spinifex-ocibsd.rules", "etc/udev/rules.d/99-spinifex-ocibsd.rules"},
	{"spinifex-daemon.service.d/ocibsd.conf", "etc/systemd/system/spinifex-daemon.service.d/ocibsd.conf"},
}

// viperblockUnit is masked on an ocibsd node, so exactly one service answers
// ebs.provider.v1.* there.
const viperblockUnit = "etc/systemd/system/spinifex-viperblock.service"

// ocibsdActivation installs the staged ocibsd files under Root and runs the
// systemd and udev steps through Runner.
type ocibsdActivation struct {
	Root   string
	Stage  string
	Runner ebsocibsd.CommandRunner
}

func productionOCIBSDActivation() ocibsdActivation {
	return ocibsdActivation{Root: "/", Stage: ocibsdStageDir, Runner: ebsocibsd.ExecRunner{}}
}

// activateEBSProvider readies an ocibsd node before spinifex.target starts;
// any other provider's node is left as setup.sh installed it.
func activateEBSProvider(ctx context.Context, e nodeEBS, a ocibsdActivation) error {
	if e.Provider != config.EBSProviderOCIBSD {
		return nil
	}
	for _, f := range ocibsdFiles {
		if err := installStaged(filepath.Join(a.Stage, f.staged), filepath.Join(a.Root, f.installed)); err != nil {
			return err
		}
	}
	if err := maskUnit(filepath.Join(a.Root, viperblockUnit)); err != nil {
		return err
	}
	for _, cmd := range [][]string{
		{"systemctl", "enable", "spinifex-ocibsd.service"},
		{"systemctl", "daemon-reload"},
		{"udevadm", "control", "--reload-rules"},
	} {
		if _, err := a.Runner.Run(ctx, cmd[0], cmd[1:]...); err != nil {
			return fmt.Errorf("activate ocibsd: %w", err)
		}
	}
	fmt.Println("✅ ocibsd installed: spinifex-ocibsd.service enabled, spinifex-viperblock.service masked")
	return nil
}

// installStaged copies src to dst with mode 0644, through a temporary file
// so a unit is never read half-written.
func installStaged(src, dst string) error {
	data, err := os.ReadFile(src)
	if err != nil {
		return fmt.Errorf("activate ocibsd: read the staged %s (setup.sh stages it on an OCI virtual machine): %w", src, err)
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil { //nolint:gosec // systemd and udev directories are world-readable
		return fmt.Errorf("activate ocibsd: %w", err)
	}
	tmp := dst + ".spx-tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil { //nolint:gosec // unit files and udev rules are world-readable, as setup.sh installs them
		return fmt.Errorf("activate ocibsd: %w", err)
	}
	if err := os.Rename(tmp, dst); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("activate ocibsd: %w", err)
	}
	return nil
}

// maskUnit links path to /dev/null. systemctl mask refuses a real unit file
// in /etc, which is where setup.sh installs the viperblock unit.
func maskUnit(path string) error {
	if target, err := os.Readlink(path); err == nil && target == os.DevNull {
		return nil
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("activate ocibsd: mask %s: %w", path, err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil { //nolint:gosec // the systemd unit directory is world-readable
		return fmt.Errorf("activate ocibsd: %w", err)
	}
	if err := os.Symlink(os.DevNull, path); err != nil {
		return fmt.Errorf("activate ocibsd: mask %s: %w", path, err)
	}
	return nil
}

func exitOnActivationError(err error) {
	if err != nil {
		fmt.Fprintf(os.Stderr, "❌ Error: %v\n", err)
		os.Exit(1)
	}
}
