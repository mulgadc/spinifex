package ocinet

import (
	"github.com/mulgadc/spinifex/spinifex/cloud/oci"
	"github.com/mulgadc/spinifex/spinifex/network/external"
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
