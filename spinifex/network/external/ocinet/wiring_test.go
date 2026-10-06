package ocinet_test

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mulgadc/spinifex/spinifex/cloud/oci"
	"github.com/mulgadc/spinifex/spinifex/network/external"
	"github.com/mulgadc/spinifex/spinifex/network/external/ocinet"
)

// A pool naming oci_auth="instance_principal" must not read a key file. It did
// for a release: the allocator failed with "open ~/.oci/config: no such file or
// directory" on a node that had been given no key on purpose.
func TestNewClientSelectsTheCredentialThePoolNames(t *testing.T) {
	for _, tc := range []struct {
		name          string
		auth          string
		configFile    string
		profile       string
		wantPrincipal bool
	}{
		{
			name:          "instance principal",
			auth:          external.OCIAuthInstancePrincipal,
			wantPrincipal: true,
		},
		{
			// A pool configured for an instance principal still must not fall
			// back to a key file when one happens to be configured as well.
			name:          "instance principal with a config file also set",
			auth:          external.OCIAuthInstancePrincipal,
			configFile:    "/etc/spinifex/oci/config",
			profile:       "spinifex",
			wantPrincipal: true,
		},
		{
			name:       "config file",
			auth:       external.OCIAuthConfigFile,
			configFile: "/etc/spinifex/oci/config",
			profile:    "spinifex",
		},
		{
			// The default, so a pool written before oci_auth existed keeps
			// authenticating from its key file.
			name:       "unset auth defaults to the config file",
			configFile: "/etc/spinifex/oci/config",
			profile:    "spinifex",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var principalCalls, configFileCalls int
			var gotPath, gotProfile string
			restore := ocinet.SwapClientConstructors(
				func() (oci.Client, error) {
					principalCalls++
					return oci.NewFake(), nil
				},
				func(path, profile string) (oci.Client, error) {
					configFileCalls++
					gotPath, gotProfile = path, profile
					return oci.NewFake(), nil
				},
			)
			defer restore()

			client, err := ocinet.NewClient(ocinet.PoolWithAuth(tc.auth, tc.configFile, tc.profile))
			require.NoError(t, err)
			require.NotNil(t, client)

			if tc.wantPrincipal {
				assert.Equal(t, 1, principalCalls, "should authenticate as the instance")
				assert.Zero(t, configFileCalls, "must not read a key file")
				return
			}
			assert.Equal(t, 1, configFileCalls, "should authenticate from the key file")
			assert.Zero(t, principalCalls)
			assert.Equal(t, tc.configFile, gotPath, "the pool's oci_config_file should reach the SDK")
			assert.Equal(t, tc.profile, gotProfile, "the pool's oci_config_profile should reach the SDK")
		})
	}
}

// The error has to name the pool: a node carries one OCI pool today and may carry
// several, and "no such file or directory" alone says nothing about which.
func TestNewClientReturnsTheConstructorError(t *testing.T) {
	wantErr := errors.New("no such file or directory")
	restore := ocinet.SwapClientConstructors(
		func() (oci.Client, error) { return nil, errors.New("not on an instance") },
		func(string, string) (oci.Client, error) { return nil, wantErr },
	)
	defer restore()

	_, err := ocinet.NewClient(ocinet.PoolWithAuth(external.OCIAuthConfigFile, "/missing", "spinifex"))
	require.ErrorIs(t, err, wantErr)
}
