package cli

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/viper"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mulgadc/spinifex/spinifex/bootstrap/config"
	"github.com/mulgadc/spinifex/spinifex/operator/admin"
	"github.com/mulgadc/spinifex/spinifex/providers/cloud/oci"
	"github.com/mulgadc/spinifex/spinifex/runtime/formation"
)

const (
	testOCICompartment = "ocid1.compartment.oc1..test"
	testOCIAD          = "Uocm:US-ASHBURN-AD-1"
	testClusterID      = "5f0c3a9e1b7d42c68e2a4f1d9b3c7e05"
)

var errRefused = errors.New("oci ListVolumes: Authorization failed or requested resource not found (NotAuthorizedOrNotFound, http 404)")

// fakeResolver is an ebsResolver over a staged environment, instance
// metadata and OCI client, counting the metadata reads it answers.
type fakeResolver struct {
	env       map[string]string
	md        *oci.InstanceMetadata
	client    *oci.BlockFake
	imdsReads int
	clientFor []string
}

func newFakeResolver() *fakeResolver {
	return &fakeResolver{env: map[string]string{}, client: oci.NewBlockFake()}
}

func (f *fakeResolver) onOCI(shape string) *fakeResolver {
	f.md = &oci.InstanceMetadata{ID: "ocid1.instance.oc1..node", CompartmentID: testOCICompartment, AvailabilityDomain: testOCIAD, Shape: shape}
	return f
}

func (f *fakeResolver) resolver() ebsResolver {
	return ebsResolver{
		getenv: func(k string) string { return f.env[k] },
		instance: func(ctx context.Context) (oci.InstanceMetadata, error) {
			f.imdsReads++
			if _, ok := ctx.Deadline(); !ok {
				return oci.InstanceMetadata{}, errors.New("the metadata read has no timeout")
			}
			if f.md == nil {
				return oci.InstanceMetadata{}, errors.New("dial tcp 169.254.169.254:80: i/o timeout")
			}
			return *f.md, nil
		},
		newClient: func(auth, file, profile string) (oci.BlockClient, error) {
			f.clientFor = append(f.clientFor, strings.Join([]string{auth, file, profile}, "|"))
			return f.client, nil
		},
	}
}

// writeExisting writes a spinifex.toml holding only node1's EBS tables.
func writeExisting(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "spinifex.toml")
	require.NoError(t, os.WriteFile(path, []byte(body), 0o600))
	return path
}

func absentConfig(t *testing.T) string {
	return filepath.Join(t.TempDir(), "spinifex.toml")
}

func TestResolveInitEBS_Precedence(t *testing.T) {
	cases := []struct {
		name     string
		existing string
		env      string
		shape    string
		want     string
		source   string
	}{
		{name: "an existing config that omits the provider keeps viperblockd over the environment and detection", existing: "[nodes.node1.daemon]\nhost = \"x\"\n", env: config.EBSProviderOCIBSD, shape: "VM.Standard.E6.Flex", want: config.EBSProviderViperblockd, source: ebsFromConfig},
		{name: "an existing config's provider wins over the environment", existing: "[nodes.node1.ebs]\nprovider = \"qemunbd\"\n", env: config.EBSProviderViperblockd, shape: "VM.Standard.E6.Flex", want: config.EBSProviderQEMUNBD, source: ebsFromConfig},
		{name: "the environment wins over detection", env: config.EBSProviderViperblockd, shape: "VM.Standard.E6.Flex", want: config.EBSProviderViperblockd, source: ebsFromEnv},
		{name: "detection on an OCI virtual machine", shape: "VM.Standard.E6.Flex", want: config.EBSProviderOCIBSD, source: ebsFromDetection},
		{name: "detection on an OCI bare-metal shape", shape: "BM.Standard.E2.64", want: config.EBSProviderViperblockd, source: ebsFromDetection},
		{name: "detection off OCI", want: config.EBSProviderViperblockd, source: ebsFromDetection},
		{name: "another node's table is not this node's config", existing: "[nodes.node2.ebs]\nprovider = \"qemunbd\"\n", want: config.EBSProviderViperblockd, source: ebsFromDetection},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeResolver()
			if tc.shape != "" {
				f.onOCI(tc.shape)
			}
			if tc.env != "" {
				f.env[ebsProviderEnv] = tc.env
			}
			path := absentConfig(t)
			if tc.existing != "" {
				path = writeExisting(t, tc.existing)
			}
			got, err := resolveInitEBS(context.Background(), path, "node1", f.resolver())
			require.NoError(t, err)
			assert.Equal(t, tc.want, got.Provider)
			assert.Equal(t, tc.source, got.Source)
			if tc.source != ebsFromDetection {
				assert.Zero(t, f.imdsReads, "a configured provider never reads the metadata service")
			}
			if tc.want != config.EBSProviderOCIBSD {
				assert.Equal(t, formation.EBSConfig{Provider: tc.want}, got.EBSConfig, "no OCI table or cluster ID off ocibsd")
				assert.Empty(t, f.clientFor, "no OCI call off ocibsd")
			}
		})
	}
}

func TestResolveInitEBS_DetectionFillsTheOCITable(t *testing.T) {
	f := newFakeResolver().onOCI("VM.Standard.E6.Flex")
	got, err := resolveInitEBS(context.Background(), absentConfig(t), "node1", f.resolver())
	require.NoError(t, err)

	assert.Equal(t, config.EBSProviderOCIBSD, got.Provider)
	assert.Equal(t, testOCICompartment, got.OCICompartmentID)
	assert.Equal(t, testOCIAD, got.OCIAvailabilityDomain)
	assert.Equal(t, "instance_principal", got.Auth)
	assert.Empty(t, got.ConfigFile)
	assert.Regexp(t, `^[0-9a-f]{32}$`, got.ClusterID, "128 random bits in hex")
	assert.Equal(t, 1, f.imdsReads, "detection's read also fills the table")
	assert.Equal(t, []string{"instance_principal||"}, f.clientFor, "the credential is checked as instance principal")
	assert.Equal(t, "EBS provider: ocibsd (detected: OCI virtual machine VM.Standard.E6.Flex)", got.describe())

	again, err := resolveInitEBS(context.Background(), absentConfig(t), "node1", f.resolver())
	require.NoError(t, err)
	assert.NotEqual(t, got.ClusterID, again.ClusterID, "each new cluster gets its own ID")
}

// An existing config's [nodes.<node>.ebs.oci] values win over the metadata
// and the instance_principal default, and its cluster ID is kept.
func TestResolveInitEBS_ExistingOCIValuesWin(t *testing.T) {
	f := newFakeResolver().onOCI("VM.Standard.E6.Flex")
	path := writeExisting(t, `[nodes.node1.ebs]
provider = "ocibsd"

[nodes.node1.ebs.oci]
compartment_id = "ocid1.compartment.oc1..mine"
cluster_id = "`+testClusterID+`"
auth = "config_file"
config_file = "/etc/spinifex/oci/config"
config_profile = "spinifex"
`)
	got, err := resolveInitEBS(context.Background(), path, "node1", f.resolver())
	require.NoError(t, err)

	assert.Equal(t, ebsFromConfig, got.Source)
	assert.Equal(t, "ocid1.compartment.oc1..mine", got.OCICompartmentID)
	assert.Equal(t, testOCIAD, got.OCIAvailabilityDomain, "a value the config omits comes from the metadata")
	assert.Equal(t, testClusterID, got.ClusterID, "a re-init keeps the ID its volumes are tagged with")
	assert.Equal(t, []string{"config_file|/etc/spinifex/oci/config|spinifex"}, f.clientFor)
}

func TestResolveInitEBS_OCIBSDFromTheEnvironmentNeedsTheMetadata(t *testing.T) {
	f := newFakeResolver()
	f.env[ebsProviderEnv] = config.EBSProviderOCIBSD
	_, err := resolveInitEBS(context.Background(), absentConfig(t), "node1", f.resolver())
	require.ErrorContains(t, err, "needs this node's OCI instance metadata")

	f.onOCI("BM.Standard.E2.64")
	got, err := resolveInitEBS(context.Background(), absentConfig(t), "node1", f.resolver())
	require.NoError(t, err, "a named provider is the operator's choice, whatever the shape")
	assert.Equal(t, ebsFromEnv, got.Source)
	assert.Equal(t, testOCIAD, got.OCIAvailabilityDomain)
}

func TestResolveInitEBS_Refusals(t *testing.T) {
	t.Run("an unknown provider in the environment", func(t *testing.T) {
		f := newFakeResolver()
		f.env[ebsProviderEnv] = "ceph"
		_, err := resolveInitEBS(context.Background(), absentConfig(t), "node1", f.resolver())
		require.ErrorContains(t, err, `EBS provider "ceph" (from SPINIFEX_EBS_PROVIDER) is unsupported`)
	})
	t.Run("an existing config that cannot be parsed", func(t *testing.T) {
		f := newFakeResolver().onOCI("VM.Standard.E6.Flex")
		_, err := resolveInitEBS(context.Background(), writeExisting(t, "[nodes.node1.ebs\n"), "node1", f.resolver())
		require.ErrorContains(t, err, "parse the existing config")
		assert.Zero(t, f.imdsReads, "an unreadable config is not replaced by detection")
	})
}

// The check is the read ocibsd makes at start: it stops init naming the
// policy when refused, and an inspect-only credential passes it.
func TestResolveInitEBS_CredentialCheck(t *testing.T) {
	t.Run("refused", func(t *testing.T) {
		f := newFakeResolver().onOCI("VM.Standard.E6.Flex")
		f.client.FailOp("ListVolumes", errRefused)
		_, err := resolveInitEBS(context.Background(), absentConfig(t), "node1", f.resolver())
		require.Error(t, err)
		assert.ErrorIs(t, err, errRefused)
		assert.ErrorContains(t, err, "manage volume-family in compartment id "+testOCICompartment)
		assert.ErrorContains(t, err, "use instances in compartment id "+testOCICompartment)
	})
	t.Run("inspect only", func(t *testing.T) {
		f := newFakeResolver().onOCI("VM.Standard.E6.Flex")
		for _, op := range []string{"CreateVolume", "AttachVolume", "CreateVolumeBackup"} {
			f.client.FailOp(op, errRefused)
		}
		got, err := resolveInitEBS(context.Background(), absentConfig(t), "node1", f.resolver())
		require.NoError(t, err)
		assert.Equal(t, config.EBSProviderOCIBSD, got.Provider)
		_, err = f.client.CreateVolume(context.Background(), oci.CreateVolumeInput{CompartmentID: testOCICompartment, AvailabilityDomain: testOCIAD, SizeGB: oci.MinVolumeSizeGB})
		assert.ErrorIs(t, err, errRefused, "the check made no create")
		assert.Empty(t, f.client.Volumes())
	})
	t.Run("ok", func(t *testing.T) {
		f := newFakeResolver().onOCI("VM.Standard.E6.Flex")
		got, err := resolveInitEBS(context.Background(), absentConfig(t), "node1", f.resolver())
		require.NoError(t, err)
		assert.Equal(t, config.EBSProviderOCIBSD, got.Provider)
	})
}

func TestResolveJoinEBS(t *testing.T) {
	ocibsdFormation := &formation.NetworkConfig{EBS: &formation.EBSConfig{
		Provider: config.EBSProviderOCIBSD, OCICompartmentID: testOCICompartment, OCIAvailabilityDomain: testOCIAD, ClusterID: testClusterID,
	}}

	t.Run("a viperblock cluster checks nothing", func(t *testing.T) {
		f := newFakeResolver().onOCI("VM.Standard.E6.Flex")
		got, err := resolveJoinEBS(context.Background(), absentConfig(t), "node2", &formation.NetworkConfig{}, f.resolver())
		require.NoError(t, err)
		assert.Equal(t, nodeEBS{Source: ebsFromFormation}, got)
		assert.Zero(t, f.imdsReads)
		assert.Empty(t, f.clientFor)
	})
	t.Run("ocibsd defaults this node to its instance principal", func(t *testing.T) {
		f := newFakeResolver()
		got, err := resolveJoinEBS(context.Background(), absentConfig(t), "node2", ocibsdFormation, f.resolver())
		require.NoError(t, err)
		assert.Equal(t, *ocibsdFormation.EBS, got.EBSConfig, "the cluster's values come from formation unchanged")
		assert.Equal(t, "instance_principal", got.Auth)
		assert.Zero(t, f.imdsReads, "formation carries the compartment and AD")
		assert.Equal(t, []string{"instance_principal||"}, f.clientFor)
	})
	t.Run("this node's existing credential wins", func(t *testing.T) {
		f := newFakeResolver()
		path := writeExisting(t, "[nodes.node2.ebs]\nprovider = \"ocibsd\"\n\n[nodes.node2.ebs.oci]\nauth = \"config_file\"\nconfig_file = \"/etc/spinifex/oci/config\"\ncluster_id = \"0000\"\n")
		got, err := resolveJoinEBS(context.Background(), path, "node2", ocibsdFormation, f.resolver())
		require.NoError(t, err)
		assert.Equal(t, "config_file", got.Auth)
		assert.Equal(t, "/etc/spinifex/oci/config", got.ConfigFile)
		assert.Equal(t, testClusterID, got.ClusterID, "the cluster ID is the cluster's, not a stale local one")
	})
	t.Run("a refused credential stops the join", func(t *testing.T) {
		f := newFakeResolver()
		f.client.FailOp("ListVolumes", errRefused)
		_, err := resolveJoinEBS(context.Background(), absentConfig(t), "node2", ocibsdFormation, f.resolver())
		require.ErrorContains(t, err, "manage volume-family")
	})
}

// fakeRunner records each command it is given.
type fakeRunner struct{ calls []string }

func (r *fakeRunner) Run(_ context.Context, name string, args ...string) ([]byte, error) {
	r.calls = append(r.calls, strings.Join(append([]string{name}, args...), " "))
	return nil, nil
}

// stageOCIBSD stages the repository's build/ocibsd files as setup.sh does.
func stageOCIBSD(t *testing.T) string {
	t.Helper()
	stage := filepath.Join(t.TempDir(), "ocibsd")
	for _, f := range ocibsdFiles {
		data, err := os.ReadFile(filepath.Join("..", "..", "..", "build", "ocibsd", f.staged))
		require.NoError(t, err)
		require.NoError(t, os.MkdirAll(filepath.Dir(filepath.Join(stage, f.staged)), 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(stage, f.staged), data, 0o644))
	}
	return stage
}

func TestActivateEBSProvider_OCIBSD(t *testing.T) {
	root := t.TempDir()
	stage := stageOCIBSD(t)
	unitDir := filepath.Join(root, "etc", "systemd", "system")
	require.NoError(t, os.MkdirAll(unitDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(unitDir, "spinifex-viperblock.service"), []byte("[Unit]\n"), 0o644))
	runner := &fakeRunner{}

	e := nodeEBS{EBSConfig: formation.EBSConfig{Provider: config.EBSProviderOCIBSD}}
	require.NoError(t, activateEBSProvider(context.Background(), e, ocibsdActivation{Root: root, Stage: stage, Runner: runner}))

	for _, f := range ocibsdFiles {
		want, err := os.ReadFile(filepath.Join(stage, f.staged))
		require.NoError(t, err)
		got, err := os.ReadFile(filepath.Join(root, f.installed))
		require.NoError(t, err, f.installed)
		assert.Equal(t, string(want), string(got), f.installed)
	}
	target, err := os.Readlink(filepath.Join(unitDir, "spinifex-viperblock.service"))
	require.NoError(t, err, "the viperblock unit file is replaced by a mask")
	assert.Equal(t, "/dev/null", target)
	assert.Equal(t, []string{
		"systemctl enable spinifex-ocibsd.service",
		"systemctl daemon-reload",
		"udevadm control --reload-rules",
	}, runner.calls)

	// A repeat, as an init --force makes, refreshes the files and keeps the mask.
	runner.calls = nil
	require.NoError(t, activateEBSProvider(context.Background(), e, ocibsdActivation{Root: root, Stage: stage, Runner: runner}))
	target, err = os.Readlink(filepath.Join(unitDir, "spinifex-viperblock.service"))
	require.NoError(t, err)
	assert.Equal(t, "/dev/null", target)
	assert.Len(t, runner.calls, 3)
}

func TestActivateEBSProvider_OtherProvidersAreUntouched(t *testing.T) {
	for _, provider := range []string{"", config.EBSProviderViperblockd, config.EBSProviderQEMUNBD} {
		root := t.TempDir()
		runner := &fakeRunner{}
		e := nodeEBS{EBSConfig: formation.EBSConfig{Provider: provider}}
		require.NoError(t, activateEBSProvider(context.Background(), e, ocibsdActivation{Root: root, Stage: filepath.Join(root, "absent"), Runner: runner}))
		assert.Empty(t, runner.calls, provider)
		entries, err := os.ReadDir(root)
		require.NoError(t, err)
		assert.Empty(t, entries, provider)
	}
}

func TestActivateEBSProvider_MissingStage(t *testing.T) {
	runner := &fakeRunner{}
	e := nodeEBS{EBSConfig: formation.EBSConfig{Provider: config.EBSProviderOCIBSD}}
	err := activateEBSProvider(context.Background(), e, ocibsdActivation{Root: t.TempDir(), Stage: filepath.Join(t.TempDir(), "absent"), Runner: runner})
	require.ErrorContains(t, err, "setup.sh stages it on an OCI virtual machine")
	assert.Empty(t, runner.calls, "nothing is enabled without its files")
}

// A viperblock cluster's formation payload is the JSON it was before the
// provider choice existed: no ebs object at all.
func TestFormationEBS_ViperblockPayloadIsUnchanged(t *testing.T) {
	cases := []struct {
		name, provider string
	}{
		{name: "viperblockd", provider: config.EBSProviderViperblockd},
		{name: "an empty provider", provider: ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ebs := formationEBS(formation.EBSConfig{Provider: tc.provider})
			assert.Nil(t, ebs)

			minimal, err := json.Marshal(formation.NetworkConfig{IPSecEnabled: true, EBS: ebs})
			require.NoError(t, err)
			assert.Equal(t, `{"external_mode":"","ipsec_enabled":true}`, string(minimal))

			external, err := json.Marshal(formation.NetworkConfig{
				ExternalMode: "nat", BootstrapVpcId: "vpc-1", BootstrapCidr: "172.31.0.0/16", EBS: ebs,
			})
			require.NoError(t, err)
			assert.Equal(t, `{"external_mode":"nat","ipsec_enabled":false,"bootstrap_vpc_id":"vpc-1","bootstrap_cidr":"172.31.0.0/16"}`, string(external))
		})
	}
}

// A non-default provider is sent with no cluster ID; only ocibsd has one.
func TestFormationEBS_OtherProvidersAreSent(t *testing.T) {
	f := newFakeResolver()
	f.env[ebsProviderEnv] = config.EBSProviderQEMUNBD
	qemu, err := resolveInitEBS(context.Background(), absentConfig(t), "node1", f.resolver())
	require.NoError(t, err)
	wire, err := json.Marshal(formation.NetworkConfig{EBS: formationEBS(qemu.EBSConfig)})
	require.NoError(t, err)
	assert.Equal(t, `{"external_mode":"","ipsec_enabled":false,"ebs":{"provider":"qemunbd"}}`, string(wire))
}

// A joining node of a viperblock cluster receives no ebs object and writes
// the default provider with no OCI table.
func TestApplyNetworkConfig_ViperblockJoinerWritesTheDefault(t *testing.T) {
	var received formation.NetworkConfig
	require.NoError(t, json.Unmarshal([]byte(`{"external_mode":"","ipsec_enabled":false}`), &received))
	assert.Nil(t, received.EBS)

	settings := admin.ConfigSettings{}
	applyNetworkConfig(&settings, &received)
	choice, err := resolveJoinEBS(context.Background(), absentConfig(t), "node1", &received, newFakeResolver().resolver())
	require.NoError(t, err)
	choice.apply(&settings)
	assert.Empty(t, settings.EBSProvider)
	assert.Empty(t, settings.OCIClusterID)

	got := renderEBS(t, settings)
	assert.Equal(t, config.EBSProviderViperblockd, got.Provider)
	assert.Equal(t, config.EBSOCIConfig{}, got.OCI)
}

// A joining node writes the provider and cluster ID the init node chose,
// and its own instance-principal credential.
func TestApplyNetworkConfig_PropagatesTheEBSProvider(t *testing.T) {
	init, err := resolveInitEBS(context.Background(), absentConfig(t), "node1", newFakeResolver().onOCI("VM.Standard.E6.Flex").resolver())
	require.NoError(t, err)
	wire, err := json.Marshal(formation.NetworkConfig{EBS: formationEBS(init.EBSConfig)})
	require.NoError(t, err)
	assert.Contains(t, string(wire), `"cluster_id":"`+init.ClusterID+`"`)
	assert.NotContains(t, string(wire), "instance_principal", "a node's credential is its own")
	var received formation.NetworkConfig
	require.NoError(t, json.Unmarshal(wire, &received))
	require.NotNil(t, received.EBS)

	settings := admin.ConfigSettings{}
	applyNetworkConfig(&settings, &received)
	choice, err := resolveJoinEBS(context.Background(), absentConfig(t), "node1", &received, newFakeResolver().resolver())
	require.NoError(t, err)
	choice.apply(&settings)

	got := renderEBS(t, settings)
	assert.Equal(t, config.EBSProviderOCIBSD, got.Provider)
	assert.Equal(t, config.EBSOCIConfig{
		CompartmentID: testOCICompartment, AvailabilityDomain: testOCIAD, ClusterID: init.ClusterID, Auth: "instance_principal",
	}, got.OCI, "the joining node's config carries the init node's ID")
}

// renderSpinifexToml renders the spinifex.toml template for one node.
func renderSpinifexToml(t *testing.T, settings admin.ConfigSettings) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "spinifex.toml")
	settings.Node, settings.Az, settings.Region, settings.Port = "node1", "ap-southeast-2a", "ap-southeast-2", "4432"
	settings.BindIP, settings.ConfigDir = "10.11.12.1", "/etc/spinifex"
	settings.OVNNBAddr, settings.OVNSBAddr = "tcp:127.0.0.1:6641", "tcp:127.0.0.1:6642"
	require.NoError(t, admin.GenerateConfigFile(path, spinifexTomlTemplate, settings))
	return path
}

// renderEBS renders the template and loads node1's [ebs] back.
func renderEBS(t *testing.T, settings admin.ConfigSettings) config.EBSConfig {
	t.Helper()
	path := renderSpinifexToml(t, settings)
	viper.Reset()
	t.Cleanup(viper.Reset)
	cfg, err := config.LoadConfig(path)
	require.NoError(t, err)
	return cfg.Nodes["node1"].EBS
}

func TestSpinifexTomlTemplate_EBSProvider(t *testing.T) {
	cases := []struct {
		name     string
		settings admin.ConfigSettings
		want     config.EBSConfig
	}{
		{name: "unset renders viperblockd", want: config.EBSConfig{Provider: config.EBSProviderViperblockd}},
		{name: "qemunbd", settings: admin.ConfigSettings{EBSProvider: config.EBSProviderQEMUNBD}, want: config.EBSConfig{Provider: config.EBSProviderQEMUNBD}},
		{
			name: "ocibsd as the instance principal",
			settings: admin.ConfigSettings{EBSProvider: config.EBSProviderOCIBSD, OCICompartmentID: testOCICompartment, OCIAvailabilityDomain: testOCIAD,
				OCIClusterID: testClusterID, OCIAuth: "instance_principal"},
			want: config.EBSConfig{Provider: config.EBSProviderOCIBSD, OCI: config.EBSOCIConfig{
				CompartmentID: testOCICompartment, AvailabilityDomain: testOCIAD, ClusterID: testClusterID, Auth: "instance_principal",
			}},
		},
		{
			name: "ocibsd with a config file",
			settings: admin.ConfigSettings{EBSProvider: config.EBSProviderOCIBSD, OCICompartmentID: testOCICompartment, OCIAvailabilityDomain: testOCIAD,
				OCIClusterID: testClusterID, OCIAuth: "config_file", OCIConfigFile: "/etc/spinifex/oci/config", OCIConfigProfile: "spinifex"},
			want: config.EBSConfig{Provider: config.EBSProviderOCIBSD, OCI: config.EBSOCIConfig{
				CompartmentID: testOCICompartment, AvailabilityDomain: testOCIAD, ClusterID: testClusterID, Auth: "config_file", ConfigFile: "/etc/spinifex/oci/config", ConfigProfile: "spinifex",
			}},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := renderEBS(t, tc.settings)
			assert.Equal(t, tc.want.Provider, got.Provider)
			assert.Equal(t, tc.want.OCI, got.OCI)
		})
	}
}

// A viperblock init renders the same spinifex.toml as one that never chose a
// provider, so resolution and the cluster ID leave its config untouched.
func TestSpinifexTomlTemplate_ViperblockInitIsUnchanged(t *testing.T) {
	ebs, err := resolveInitEBS(context.Background(), absentConfig(t), "node1", newFakeResolver().resolver())
	require.NoError(t, err)
	settings := admin.ConfigSettings{}
	ebs.apply(&settings)

	before, err := os.ReadFile(renderSpinifexToml(t, admin.ConfigSettings{}))
	require.NoError(t, err)
	after, err := os.ReadFile(renderSpinifexToml(t, settings))
	require.NoError(t, err)
	assert.Equal(t, string(before), string(after))
	assert.NotContains(t, string(after), "cluster_id")
	assert.NotContains(t, string(after), ".ebs.oci]")
}

// What init renders for ocibsd is what a re-init reads back as the existing
// config, so a --force keeps the provider, the table and the cluster ID.
func TestReadExistingEBS_RoundTrip(t *testing.T) {
	path := renderSpinifexToml(t, admin.ConfigSettings{
		EBSProvider: config.EBSProviderOCIBSD, OCICompartmentID: testOCICompartment, OCIAvailabilityDomain: testOCIAD, OCIClusterID: testClusterID, OCIAuth: "instance_principal",
	})
	f := newFakeResolver().onOCI("VM.Standard.E6.Flex")
	f.env[ebsProviderEnv] = config.EBSProviderViperblockd
	got, err := resolveInitEBS(context.Background(), path, "node1", f.resolver())
	require.NoError(t, err)
	assert.Equal(t, ebsFromConfig, got.Source)
	assert.Equal(t, formation.EBSConfig{
		Provider: config.EBSProviderOCIBSD, OCICompartmentID: testOCICompartment, OCIAvailabilityDomain: testOCIAD, ClusterID: testClusterID,
	}, got.EBSConfig)
	assert.Equal(t, "instance_principal", got.Auth)
	assert.Zero(t, f.imdsReads, "a complete existing table needs no metadata")

	viperblock, err := readExistingEBS(renderSpinifexToml(t, admin.ConfigSettings{}), "node1")
	require.NoError(t, err)
	require.NotNil(t, viperblock)
	assert.Equal(t, config.EBSProviderViperblockd, viperblock.Provider)

	absent, err := readExistingEBS(absentConfig(t), "node1")
	require.NoError(t, err)
	assert.Nil(t, absent)
}
