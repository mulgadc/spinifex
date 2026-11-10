package cli

import (
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
)

func TestEBSInitConfig(t *testing.T) {
	cases := []struct {
		name, provider, compartment, ad string
		want                            formation.EBSConfig
		wantErr                         string
	}{
		{name: "the default", provider: config.EBSProviderViperblockd, want: formation.EBSConfig{Provider: config.EBSProviderViperblockd}},
		{name: "qemunbd", provider: config.EBSProviderQEMUNBD, want: formation.EBSConfig{Provider: config.EBSProviderQEMUNBD}},
		{name: "ocibsd", provider: config.EBSProviderOCIBSD, compartment: testOCICompartment, ad: testOCIAD,
			want: formation.EBSConfig{Provider: config.EBSProviderOCIBSD, OCICompartmentID: testOCICompartment, OCIAvailabilityDomain: testOCIAD}},
		{name: "an unknown provider", provider: "ceph", wantErr: `--ebs-provider "ceph" unsupported`},
		{name: "ocibsd without a compartment", provider: config.EBSProviderOCIBSD, ad: testOCIAD, wantErr: "requires --oci-compartment-id and --oci-availability-domain"},
		{name: "ocibsd without an AD", provider: config.EBSProviderOCIBSD, compartment: testOCICompartment, wantErr: "requires --oci-compartment-id and --oci-availability-domain"},
		{name: "OCI flags on another provider", provider: config.EBSProviderViperblockd, ad: testOCIAD, wantErr: "only valid with --ebs-provider ocibsd"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ebsInitConfig(tc.provider, tc.compartment, tc.ad)
			if tc.wantErr != "" {
				require.ErrorContains(t, err, tc.wantErr)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}
}

// A joining node writes the provider the init node chose, not its own.
func TestApplyNetworkConfig_PropagatesTheEBSProvider(t *testing.T) {
	settings := &admin.ConfigSettings{}
	applyNetworkConfig(settings, &formation.NetworkConfig{EBS: formation.EBSConfig{
		Provider: config.EBSProviderOCIBSD, OCICompartmentID: testOCICompartment, OCIAvailabilityDomain: testOCIAD,
	}})
	assert.Equal(t, config.EBSProviderOCIBSD, settings.EBSProvider)
	assert.Equal(t, testOCICompartment, settings.OCICompartmentID)
	assert.Equal(t, testOCIAD, settings.OCIAvailabilityDomain)
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
			settings: admin.ConfigSettings{EBSProvider: config.EBSProviderOCIBSD, OCICompartmentID: testOCICompartment, OCIAvailabilityDomain: testOCIAD},
			want: config.EBSConfig{Provider: config.EBSProviderOCIBSD, OCI: config.EBSOCIConfig{
				CompartmentID: testOCICompartment, AvailabilityDomain: testOCIAD,
			}},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "spinifex.toml")
			settings := tc.settings
			settings.Node, settings.Az, settings.Region, settings.Port = "node1", "ap-southeast-2a", "ap-southeast-2", "4432"
			settings.BindIP, settings.ConfigDir = "10.11.12.1", dir
			settings.OVNNBAddr, settings.OVNSBAddr = "tcp:127.0.0.1:6641", "tcp:127.0.0.1:6642"
			require.NoError(t, admin.GenerateConfigFile(path, spinifexTomlTemplate, settings))

			viper.Reset()
			t.Cleanup(viper.Reset)
			cfg, err := config.LoadConfig(path)
			require.NoError(t, err)
			got := cfg.Nodes["node1"].EBS
			assert.Equal(t, tc.want.Provider, got.Provider)
			assert.Equal(t, tc.want.OCI, got.OCI)
		})
	}
}
