package cli

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"

	toml "github.com/pelletier/go-toml/v2"

	"github.com/mulgadc/spinifex/spinifex/bootstrap/config"
	"github.com/mulgadc/spinifex/spinifex/runtime/formation"
)

// ebsInitConfig checks the init flags that choose the cluster's EBS provider,
// which every node is written with: the OCI flags go with ocibsd and only it.
// ocibsd also gets a cluster ID, priorClusterID when a re-init preserves one.
func ebsInitConfig(provider, compartmentID, availabilityDomain, priorClusterID string) (formation.EBSConfig, error) {
	if !config.SupportedEBSProvider(provider) {
		return formation.EBSConfig{}, fmt.Errorf("--ebs-provider %q unsupported; use %q, %q or %q", provider, config.EBSProviderViperblockd, config.EBSProviderQEMUNBD, config.EBSProviderOCIBSD)
	}
	if provider != config.EBSProviderOCIBSD {
		if compartmentID != "" || availabilityDomain != "" {
			return formation.EBSConfig{}, fmt.Errorf("--oci-compartment-id and --oci-availability-domain are only valid with --ebs-provider %s", config.EBSProviderOCIBSD)
		}
		return formation.EBSConfig{Provider: provider}, nil
	}
	if compartmentID == "" || availabilityDomain == "" {
		return formation.EBSConfig{}, fmt.Errorf("--ebs-provider %s requires --oci-compartment-id and --oci-availability-domain", config.EBSProviderOCIBSD)
	}
	clusterID := priorClusterID
	if clusterID == "" {
		var err error
		if clusterID, err = generateClusterID(); err != nil {
			return formation.EBSConfig{}, err
		}
	}
	return formation.EBSConfig{
		Provider:              provider,
		OCICompartmentID:      compartmentID,
		OCIAvailabilityDomain: availabilityDomain,
		ClusterID:             clusterID,
	}, nil
}

// generateClusterID returns a random 128-bit value in hex.
func generateClusterID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("generate the cluster ID: %w", err)
	}
	return hex.EncodeToString(b[:]), nil
}

// readOCIClusterID returns node's [nodes.<node>.ebs.oci] cluster_id from an
// existing spinifex.toml, so a --force re-init keeps the tag on its volumes;
// "" when the file, the table or the key is absent.
func readOCIClusterID(tomlPath, node string) string {
	raw, err := os.ReadFile(tomlPath)
	if err != nil {
		return ""
	}
	var cfg struct {
		Nodes map[string]struct {
			EBS struct {
				OCI struct {
					ClusterID string `toml:"cluster_id"`
				} `toml:"oci"`
			} `toml:"ebs"`
		} `toml:"nodes"`
	}
	if err := toml.Unmarshal(raw, &cfg); err != nil {
		return ""
	}
	return cfg.Nodes[node].EBS.OCI.ClusterID
}
