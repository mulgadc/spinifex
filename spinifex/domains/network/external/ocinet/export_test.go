package ocinet

import (
	"github.com/mulgadc/spinifex/spinifex/domains/network/external"
	"github.com/mulgadc/spinifex/spinifex/providers/cloud/oci"
)

// NewClient is the credential choice a pool makes, which is worth testing on its
// own: both real constructors need an instance or a key file, so the branch is
// otherwise observable only on OCI.
var NewClient = newClient

// SwapClientConstructors replaces both so a test can see which one a pool selects,
// and returns a function restoring them.
func SwapClientConstructors(
	principal func() (oci.Client, error),
	configFile func(path, profile string) (oci.Client, error),
) func() {
	savedPrincipal, savedConfigFile := newInstancePrincipalClient, newConfigFileClient
	newInstancePrincipalClient, newConfigFileClient = principal, configFile
	return func() {
		newInstancePrincipalClient, newConfigFileClient = savedPrincipal, savedConfigFile
	}
}

// PoolWithAuth is the smallest source="oci" pool that reaches the credential
// choice, so a test states the auth it means and nothing else.
func PoolWithAuth(auth, configFile, profile string) external.ExternalPoolConfig {
	return external.ExternalPoolConfig{
		Name:             "oci-public",
		Source:           external.SourceOCI,
		OCIAuth:          auth,
		OCIConfigFile:    configFile,
		OCIConfigProfile: profile,
	}
}

// ReportAuthorisation logs the line validate-topology.sh gates the whole OCI
// suite on, so a test pins the exact text and not just the behaviour.
var ReportAuthorisation = reportAuthorisation
