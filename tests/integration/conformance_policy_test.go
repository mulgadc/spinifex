//go:build integration

package integration

import (
	"testing"

	"github.com/mulgadc/spinifex/internal/awsmodel"
	"github.com/stretchr/testify/require"
)

func TestEmbeddedConformancePolicyPromotesSTS(t *testing.T) {
	policy, err := loadConformancePolicy()
	require.NoError(t, err)
	require.Equal(t, []awsmodel.Service{awsmodel.STS}, policy.services())
	require.True(t, policy.isPromoted(awsmodel.STS))
	require.False(t, policy.isPromoted(awsmodel.EC2))
}

func TestConformanceModeFromEnvironment(t *testing.T) {
	tests := []struct {
		name    string
		value   string
		want    conformanceMode
		wantErr bool
	}{
		{name: "default is fail", want: conformanceModeFail},
		{name: "explicit fail", value: "fail", want: conformanceModeFail},
		{name: "warn", value: "warn", want: conformanceModeWarn},
		{name: "case and whitespace", value: " WARN ", want: conformanceModeWarn},
		{name: "invalid", value: "off", wantErr: true},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Setenv("AWS_MODEL_CONFORMANCE_MODE", test.value)
			got, err := conformanceModeFromEnvironment()
			if test.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			require.Equal(t, test.want, got)
		})
	}
}
