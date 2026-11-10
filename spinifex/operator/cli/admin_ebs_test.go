package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/spf13/viper"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mulgadc/spinifex/spinifex/bootstrap/config"
	"github.com/mulgadc/spinifex/spinifex/operator/admin"
	"github.com/mulgadc/spinifex/spinifex/runtime/formation"
)

const (
	testOCICompartment = "ocid1.compartment.oc1..test"
	testOCIAD          = "Uocm:US-ASHBURN-AD-1"
	testClusterID      = "5f0c3a9e1b7d42c68e2a4f1d9b3c7e05"
)

func TestEBSInitConfig(t *testing.T) {
	cases := []struct {
		name, provider, compartment, ad string
		want                            formation.EBSConfig
		wantErr                         string
	}{
		{name: "the default", provider: config.EBSProviderViperblockd, want: formation.EBSConfig{Provider: config.EBSProviderViperblockd}},
		{name: "qemunbd", provider: config.EBSProviderQEMUNBD, want: formation.EBSConfig{Provider: config.EBSProviderQEMUNBD}},
		{name: "an unknown provider", provider: "ceph", wantErr: `--ebs-provider "ceph" unsupported`},
		{name: "ocibsd without a compartment", provider: config.EBSProviderOCIBSD, ad: testOCIAD, wantErr: "requires --oci-compartment-id and --oci-availability-domain"},
		{name: "ocibsd without an AD", provider: config.EBSProviderOCIBSD, compartment: testOCICompartment, wantErr: "requires --oci-compartment-id and --oci-availability-domain"},
		{name: "OCI flags on another provider", provider: config.EBSProviderViperblockd, ad: testOCIAD, wantErr: "only valid with --ebs-provider ocibsd"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ebsInitConfig(tc.provider, tc.compartment, tc.ad, "")
			if tc.wantErr != "" {
				require.ErrorContains(t, err, tc.wantErr)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
			wire, err := json.Marshal(formation.NetworkConfig{EBS: formationEBS(got)})
			require.NoError(t, err)
			assert.NotContains(t, string(wire), "cluster_id", "no cluster ID reaches joining nodes")
		})
	}
}

// A viperblock cluster's formation payload is the JSON it was before the
// provider choice existed: no ebs object at all.
func TestFormationEBS_ViperblockPayloadIsUnchanged(t *testing.T) {
	cases := []struct {
		name, provider string
	}{
		{name: "the default flag", provider: config.EBSProviderViperblockd},
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

// A non-default provider is sent; only ocibsd carries the OCI fields.
func TestFormationEBS_OtherProvidersAreSent(t *testing.T) {
	qemu, err := ebsInitConfig(config.EBSProviderQEMUNBD, "", "", "")
	require.NoError(t, err)
	wire, err := json.Marshal(formation.NetworkConfig{EBS: formationEBS(qemu)})
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
	assert.Empty(t, settings.EBSProvider)
	assert.Empty(t, settings.OCIClusterID)

	got := renderEBS(t, settings)
	assert.Equal(t, config.EBSProviderViperblockd, got.Provider)
	assert.Equal(t, config.EBSOCIConfig{}, got.OCI)
}

func TestEBSInitConfig_OCIBSDGeneratesAClusterID(t *testing.T) {
	a, err := ebsInitConfig(config.EBSProviderOCIBSD, testOCICompartment, testOCIAD, "")
	require.NoError(t, err)
	assert.Equal(t, formation.EBSConfig{Provider: config.EBSProviderOCIBSD, OCICompartmentID: testOCICompartment, OCIAvailabilityDomain: testOCIAD, ClusterID: a.ClusterID}, a)
	assert.Regexp(t, `^[0-9a-f]{32}$`, a.ClusterID, "128 random bits in hex")

	b, err := ebsInitConfig(config.EBSProviderOCIBSD, testOCICompartment, testOCIAD, "")
	require.NoError(t, err)
	assert.NotEqual(t, a.ClusterID, b.ClusterID, "each init names a new cluster")

	again, err := ebsInitConfig(config.EBSProviderOCIBSD, testOCICompartment, testOCIAD, a.ClusterID)
	require.NoError(t, err)
	assert.Equal(t, a.ClusterID, again.ClusterID, "a re-init keeps the ID its volumes are tagged with")
}

// A joining node writes the provider and cluster ID the init node chose.
func TestApplyNetworkConfig_PropagatesTheEBSProvider(t *testing.T) {
	init, err := ebsInitConfig(config.EBSProviderOCIBSD, testOCICompartment, testOCIAD, "")
	require.NoError(t, err)
	wire, err := json.Marshal(formation.NetworkConfig{EBS: formationEBS(init)})
	require.NoError(t, err)
	assert.Contains(t, string(wire), `"cluster_id":"`+init.ClusterID+`"`)
	var received formation.NetworkConfig
	require.NoError(t, json.Unmarshal(wire, &received))
	require.NotNil(t, received.EBS)

	settings := admin.ConfigSettings{}
	applyNetworkConfig(&settings, &received)
	assert.Equal(t, config.EBSProviderOCIBSD, settings.EBSProvider)
	assert.Equal(t, testOCICompartment, settings.OCICompartmentID)
	assert.Equal(t, testOCIAD, settings.OCIAvailabilityDomain)
	assert.Equal(t, init.ClusterID, settings.OCIClusterID)

	got := renderEBS(t, settings)
	assert.Equal(t, init.ClusterID, got.OCI.ClusterID, "the joining node's config carries the init node's ID")
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
			name:     "ocibsd with its table",
			settings: admin.ConfigSettings{EBSProvider: config.EBSProviderOCIBSD, OCICompartmentID: testOCICompartment, OCIAvailabilityDomain: testOCIAD, OCIClusterID: testClusterID},
			want: config.EBSConfig{Provider: config.EBSProviderOCIBSD, OCI: config.EBSOCIConfig{
				CompartmentID: testOCICompartment, AvailabilityDomain: testOCIAD, ClusterID: testClusterID,
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
// provider, so the ocibsd flags and cluster ID leave its config untouched.
func TestSpinifexTomlTemplate_ViperblockInitIsUnchanged(t *testing.T) {
	ebs, err := ebsInitConfig(config.EBSProviderViperblockd, "", "", "")
	require.NoError(t, err)
	settings := admin.ConfigSettings{
		EBSProvider: ebs.Provider, OCICompartmentID: ebs.OCICompartmentID, OCIAvailabilityDomain: ebs.OCIAvailabilityDomain, OCIClusterID: ebs.ClusterID,
	}

	before, err := os.ReadFile(renderSpinifexToml(t, admin.ConfigSettings{}))
	require.NoError(t, err)
	after, err := os.ReadFile(renderSpinifexToml(t, settings))
	require.NoError(t, err)
	assert.Equal(t, string(before), string(after))
	assert.NotContains(t, string(after), "cluster_id")
	assert.NotContains(t, string(after), ".ebs.oci]")
}

func TestReadOCIClusterID(t *testing.T) {
	ocibsd := renderSpinifexToml(t, admin.ConfigSettings{
		EBSProvider: config.EBSProviderOCIBSD, OCICompartmentID: testOCICompartment, OCIAvailabilityDomain: testOCIAD, OCIClusterID: testClusterID,
	})
	assert.Equal(t, testClusterID, readOCIClusterID(ocibsd, "node1"))
	assert.Empty(t, readOCIClusterID(ocibsd, "node2"), "another node's table is not this node's")
	assert.Empty(t, readOCIClusterID(renderSpinifexToml(t, admin.ConfigSettings{}), "node1"), "a viperblock config has none")
	assert.Empty(t, readOCIClusterID(filepath.Join(t.TempDir(), "absent.toml"), "node1"))
}
