package handlers_eks

import (
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/eks"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// baseClusterMetaForProjection returns a minimal-but-valid ClusterMeta, used as
// the common starting point for clusterMetaToAWS table tests below.
func baseClusterMetaForProjection() *ClusterMeta {
	return &ClusterMeta{
		Name:    "alpha",
		Arn:     "arn:aws:eks:us-east-1:111122223333:cluster/alpha",
		Status:  ClusterStatusActive,
		Version: "1.32",
		RoleArn: "arn:aws:iam::111122223333:role/eks-cluster",
		ResourcesVpcConfig: &ClusterVpcConfig{
			SubnetIds: []string{"subnet-aaa"},
			VpcId:     "vpc-aaa",
		},
		CreatedAt: time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC),
	}
}

// --- A: kubernetesNetworkConfig + accessConfig (mulga-rmsr7) ---

func TestClusterMetaToAWS_KubernetesNetworkConfig(t *testing.T) {
	tests := []struct {
		name        string
		mutate      func(*ClusterMeta)
		wantIP4     string
		wantIPFam   string
		wantIPv6Set bool
	}{
		{
			name:      "zero-value record predating the field projects the real ipv4 default",
			mutate:    func(m *ClusterMeta) {},
			wantIP4:   defaultServiceIPv4CIDR,
			wantIPFam: eks.IpFamilyIpv4,
		},
		{
			name: "caller-requested serviceIpv4Cidr is echoed verbatim",
			mutate: func(m *ClusterMeta) {
				m.KubernetesNetworkConfig = &ClusterNetworkConfig{
					IpFamily:        eks.IpFamilyIpv4,
					ServiceIpv4Cidr: "172.20.0.0/16",
				}
			},
			wantIP4:   "172.20.0.0/16",
			wantIPFam: eks.IpFamilyIpv4,
		},
		{
			name: "caller-requested ipv6 family omits serviceIpv4Cidr",
			mutate: func(m *ClusterMeta) {
				m.KubernetesNetworkConfig = &ClusterNetworkConfig{IpFamily: eks.IpFamilyIpv6}
			},
			wantIPFam: eks.IpFamilyIpv6,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			meta := baseClusterMetaForProjection()
			tt.mutate(meta)

			out := clusterMetaToAWS(meta)

			require.NotNil(t, out.KubernetesNetworkConfig)
			assert.Equal(t, tt.wantIPFam, aws.StringValue(out.KubernetesNetworkConfig.IpFamily))
			if tt.wantIP4 == "" {
				assert.Nil(t, out.KubernetesNetworkConfig.ServiceIpv4Cidr)
			} else {
				assert.Equal(t, tt.wantIP4, aws.StringValue(out.KubernetesNetworkConfig.ServiceIpv4Cidr))
			}
		})
	}
}

func TestClusterMetaToAWS_AccessConfig(t *testing.T) {
	tests := []struct {
		name          string
		mutate        func(*ClusterMeta)
		wantBootstrap bool
		wantMode      string
	}{
		{
			name:          "zero-value record predating the field defaults bootstrap-admin true and mode API",
			mutate:        func(m *ClusterMeta) {},
			wantBootstrap: true,
			wantMode:      eks.AuthenticationModeApi,
		},
		{
			name: "caller explicitly requested bootstrap-admin true",
			mutate: func(m *ClusterMeta) {
				v := true
				m.BootstrapClusterCreatorAdminPermissions = &v
			},
			wantBootstrap: true,
			wantMode:      eks.AuthenticationModeApi,
		},
		{
			name: "caller explicitly disabled bootstrap-admin",
			mutate: func(m *ClusterMeta) {
				v := false
				m.BootstrapClusterCreatorAdminPermissions = &v
			},
			wantBootstrap: false,
			wantMode:      eks.AuthenticationModeApi,
		},
		{
			name: "cluster created with API_AND_CONFIG_MAP describes back the same mode",
			mutate: func(m *ClusterMeta) {
				m.AuthenticationMode = eks.AuthenticationModeApiAndConfigMap
			},
			wantBootstrap: true,
			wantMode:      eks.AuthenticationModeApiAndConfigMap,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			meta := baseClusterMetaForProjection()
			tt.mutate(meta)

			out := clusterMetaToAWS(meta)

			require.NotNil(t, out.AccessConfig)
			assert.Equal(t, tt.wantMode, aws.StringValue(out.AccessConfig.AuthenticationMode))
			assert.Equal(t, tt.wantBootstrap, aws.BoolValue(out.AccessConfig.BootstrapClusterCreatorAdminPermissions))
		})
	}
}

// --- E: resourcesVpcConfig.clusterSecurityGroupId (mulga-xhjd9) ---

func TestClusterMetaToAWS_ClusterSecurityGroupId(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*ClusterMeta)
		want   string
	}{
		{
			name:   "zero-value record predating the field projects no clusterSecurityGroupId",
			mutate: func(m *ClusterMeta) {},
			want:   "",
		},
		{
			name: "caller-VPC primary SG id is projected",
			mutate: func(m *ClusterMeta) {
				m.ResourcesVpcConfig.ClusterSecurityGroupId = "sg-primary123"
			},
			want: "sg-primary123",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			meta := baseClusterMetaForProjection()
			tt.mutate(meta)

			out := clusterMetaToAWS(meta)

			require.NotNil(t, out.ResourcesVpcConfig)
			if tt.want == "" {
				assert.Nil(t, out.ResourcesVpcConfig.ClusterSecurityGroupId)
			} else {
				assert.Equal(t, tt.want, aws.StringValue(out.ResourcesVpcConfig.ClusterSecurityGroupId))
			}
		})
	}
}

// --- F: resourcesVpcConfig.securityGroupIds honors only what the caller passed (mulga-bnf3u) ---

func TestClusterMetaToAWS_SecurityGroupIdsEchoesCallerOnly(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*ClusterMeta)
		want   []string
	}{
		{
			name:   "zero-value record predating the field projects an empty list, not platform SGs",
			mutate: func(m *ClusterMeta) {},
			want:   []string{},
		},
		{
			name: "caller-supplied security groups are echoed verbatim",
			mutate: func(m *ClusterMeta) {
				m.ResourcesVpcConfig.SecurityGroupIds = []string{"sg-caller-1", "sg-caller-2"}
			},
			want: []string{"sg-caller-1", "sg-caller-2"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			meta := baseClusterMetaForProjection()
			tt.mutate(meta)

			out := clusterMetaToAWS(meta)

			require.NotNil(t, out.ResourcesVpcConfig)
			assert.Equal(t, tt.want, aws.StringValueSlice(out.ResourcesVpcConfig.SecurityGroupIds))
		})
	}
}

// --- H: platformVersion, upgradePolicy, logging (mulga-z2knb) ---

func TestClusterMetaToAWS_PlatformVersionAlwaysPresent(t *testing.T) {
	// platformVersion is a stable constant, not derived from any stored field,
	// so an old zero-value-ish record projects it identically to a new one.
	meta := baseClusterMetaForProjection()
	out := clusterMetaToAWS(meta)
	assert.Equal(t, eksPlatformVersion, aws.StringValue(out.PlatformVersion))
}

func TestClusterMetaToAWS_UpgradePolicy(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*ClusterMeta)
		want   string
	}{
		{
			name:   "zero-value record predating the field projects the AWS-standard default",
			mutate: func(m *ClusterMeta) {},
			want:   eks.SupportTypeStandard,
		},
		{
			name: "caller-requested extended support is echoed verbatim",
			mutate: func(m *ClusterMeta) {
				m.UpgradePolicy = &ClusterUpgradePolicyMeta{SupportType: eks.SupportTypeExtended}
			},
			want: eks.SupportTypeExtended,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			meta := baseClusterMetaForProjection()
			tt.mutate(meta)

			out := clusterMetaToAWS(meta)

			require.NotNil(t, out.UpgradePolicy)
			assert.Equal(t, tt.want, aws.StringValue(out.UpgradePolicy.SupportType))
		})
	}
}

func TestClusterMetaToAWS_Logging(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*ClusterMeta)
		want   []ClusterLogSetup
	}{
		{
			name:   "zero-value record predating the field reports every log type honestly disabled",
			mutate: func(m *ClusterMeta) {},
			want:   []ClusterLogSetup{{Types: allClusterLogTypes, Enabled: false}},
		},
		{
			name: "caller-requested logging, including disabled entries, is echoed exactly",
			mutate: func(m *ClusterMeta) {
				m.Logging = []ClusterLogSetup{
					{Types: []string{eks.LogTypeApi, eks.LogTypeAudit}, Enabled: true},
					{Types: []string{eks.LogTypeAuthenticator, eks.LogTypeControllerManager, eks.LogTypeScheduler}, Enabled: false},
				}
			},
			want: []ClusterLogSetup{
				{Types: []string{eks.LogTypeApi, eks.LogTypeAudit}, Enabled: true},
				{Types: []string{eks.LogTypeAuthenticator, eks.LogTypeControllerManager, eks.LogTypeScheduler}, Enabled: false},
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			meta := baseClusterMetaForProjection()
			tt.mutate(meta)

			out := clusterMetaToAWS(meta)

			require.NotNil(t, out.Logging)
			require.Len(t, out.Logging.ClusterLogging, len(tt.want))
			for i, wantEntry := range tt.want {
				got := out.Logging.ClusterLogging[i]
				assert.Equal(t, wantEntry.Types, aws.StringValueSlice(got.Types))
				assert.Equal(t, wantEntry.Enabled, aws.BoolValue(got.Enabled))
			}
		})
	}
}
