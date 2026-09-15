package gateway_ecrapi

//test:in-package — the connection and repository seeding helpers these share
// with the rest of the package's tests are unexported, as is the request shape
// the handler decodes.

import (
	"context"
	"testing"

	"github.com/aws/aws-sdk-go/service/ecr"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestPutImageScanningConfiguration_NoOpSucceeds covers the honest,
// already-true state: scanOnPush unset or explicitly false succeeds, unlike
// the rest of the scan surface, which rejects every call unread.
func TestPutImageScanningConfiguration_NoOpSucceeds(t *testing.T) {
	nc := newPolicyTestConn(t)
	seedRepo(t, nc, "team/app")

	cases := []struct {
		name, body string
	}{
		{"absent", `{"repositoryName":"team/app"}`},
		{"explicit false", `{"repositoryName":"team/app","imageScanningConfiguration":{"scanOnPush":false}}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, err := PutImageScanningConfiguration(context.Background(), nc, policyTestAccount, []byte(tc.body))
			require.NoError(t, err)
			res, ok := out.(*ecr.PutImageScanningConfigurationOutput)
			require.True(t, ok)
			assert.Equal(t, "team/app", *res.RepositoryName)
			assert.Equal(t, policyTestAccount, *res.RegistryId)
			require.NotNil(t, res.ImageScanningConfiguration)
			assert.False(t, *res.ImageScanningConfiguration.ScanOnPush)
		})
	}
}

// TestPutImageScanningConfiguration_ScanOnPushRejected covers the real,
// narrow defect: scanOnPush=true still asks for a scanner that does not
// exist, and is rejected exactly like the rest of the scan surface.
func TestPutImageScanningConfiguration_ScanOnPushRejected(t *testing.T) {
	nc := newPolicyTestConn(t)
	seedRepo(t, nc, "team/app")

	out, err := PutImageScanningConfiguration(context.Background(), nc, policyTestAccount,
		[]byte(`{"repositoryName":"team/app","imageScanningConfiguration":{"scanOnPush":true}}`))
	assert.Nil(t, out)
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorOperationNotSupported, err.Error())
}

func TestPutImageScanningConfiguration_Errors(t *testing.T) {
	nc := newPolicyTestConn(t)
	seedRepo(t, nc, "team/app")

	cases := []struct {
		name, body, expect string
	}{
		{"missing repo", `{"repositoryName":"team/ghost"}`, awserrors.ErrorRepositoryNotFound},
		{"empty name", `{}`, awserrors.ErrorInvalidParameterValue},
		{"invalid name", `{"repositoryName":"Team/App"}`, awserrors.ErrorInvalidParameterValue},
		{"cross-account", `{"repositoryName":"team/app","registryId":"999999999999"}`, awserrors.ErrorAccessDenied},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := PutImageScanningConfiguration(context.Background(), nc, policyTestAccount, []byte(tc.body))
			require.Error(t, err)
			assert.Equal(t, tc.expect, err.Error())
		})
	}
}

// TestActions_PutImageScanningConfigurationDispatched confirms the action
// table now routes to the real handler rather than ScanningNotSupported.
func TestActions_PutImageScanningConfigurationDispatched(t *testing.T) {
	h, ok := Actions["PutImageScanningConfiguration"]
	require.True(t, ok)
	nc := newPolicyTestConn(t)
	seedRepo(t, nc, "team/app")

	out, err := h(context.Background(), nc, policyTestAccount, []byte(`{"repositoryName":"team/app"}`))
	require.NoError(t, err)
	_, ok = out.(*ecr.PutImageScanningConfigurationOutput)
	assert.True(t, ok)
}
