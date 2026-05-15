package handlers_elbv2

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// extractAgentEnv parses the cloud-config user-data into the env-var map that
// cloud-init will write to /etc/conf.d/lb-agent. We don't pull in a YAML
// dependency just for tests — the format is stable enough that line-based
// parsing catches the things substring matching misses (misplaced keys,
// broken indentation, content that escaped the agent-conf block).
//
// Returns the parsed env map plus the `runcmd:` section's body (raw lines)
// so callers can assert on the launch command shape.
func extractAgentEnv(t *testing.T, ud string) (envs map[string]string, runcmd []string) {
	t.Helper()
	require.True(t, strings.HasPrefix(ud, "#cloud-config\n"),
		"user-data must start with #cloud-config directive")

	envs = map[string]string{}
	const agentPathMarker = "  - path: /etc/conf.d/lb-agent"
	const contentMarker = "    content: |"
	const envIndent = "      " // 6 spaces under `content: |`

	lines := strings.Split(ud, "\n")
	inAgentEnv := false
	inRuncmd := false
	for i := 0; i < len(lines); i++ {
		line := lines[i]
		switch {
		case line == agentPathMarker:
			require.Less(t, i+1, len(lines), "agent path entry truncated")
			require.Equal(t, contentMarker, lines[i+1],
				"agent path must be followed by `content: |` block")
			inAgentEnv = true
			i++ // skip the content marker
		case inAgentEnv:
			if !strings.HasPrefix(line, envIndent) {
				inAgentEnv = false
				i-- // re-process this line in case it starts another section
				continue
			}
			kv := strings.TrimPrefix(line, envIndent)
			k, v, ok := strings.Cut(kv, "=")
			require.True(t, ok, "agent env line missing '=': %q", line)
			envs[k] = v
		case line == "runcmd:":
			inRuncmd = true
		case inRuncmd:
			if !strings.HasPrefix(line, "  ") {
				inRuncmd = false
				continue
			}
			runcmd = append(runcmd, line)
		}
	}
	return envs, runcmd
}

func TestBuildLBArn(t *testing.T) {
	arn := buildLBArn("us-east-1", "123456789012", "my-alb", "50dc6c495c0c9188", LoadBalancerTypeApplication)
	assert.Equal(t, "arn:aws:elasticloadbalancing:us-east-1:123456789012:loadbalancer/app/my-alb/50dc6c495c0c9188", arn)
}

func TestBuildLBArn_Network(t *testing.T) {
	arn := buildLBArn("us-east-1", "123456789012", "my-nlb", "50dc6c495c0c9188", LoadBalancerTypeNetwork)
	assert.Equal(t, "arn:aws:elasticloadbalancing:us-east-1:123456789012:loadbalancer/net/my-nlb/50dc6c495c0c9188", arn)
}

func TestBuildTGArn(t *testing.T) {
	arn := buildTGArn("us-west-2", "111222333444", "my-tg", "deadbeef")
	assert.Equal(t, "arn:aws:elasticloadbalancing:us-west-2:111222333444:targetgroup/my-tg/deadbeef", arn)
}

func TestBuildListenerArn(t *testing.T) {
	arn := buildListenerArn("eu-west-1", "999888777666", "my-alb", "lbid123", "listener456", LoadBalancerTypeApplication)
	assert.Equal(t, "arn:aws:elasticloadbalancing:eu-west-1:999888777666:listener/app/my-alb/lbid123/listener456", arn)
}

func TestBuildListenerArn_NLB(t *testing.T) {
	arn := buildListenerArn("eu-west-1", "999888777666", "my-nlb", "lbid123", "listener456", LoadBalancerTypeNetwork)
	assert.Equal(t, "arn:aws:elasticloadbalancing:eu-west-1:999888777666:listener/net/my-nlb/lbid123/listener456", arn)
}

// TestLbVMUserData_Structural parses the generated cloud-config and asserts
// the structure: agent env vars land in /etc/conf.d/lb-agent, runcmd starts
// lb-agent via OpenRC (not the bare binary), the CA cert section — owned by
// the instance service's cloud-init template — is absent, and no bootcmd is
// emitted in the single-node default.
func TestLbVMUserData_Structural(t *testing.T) {
	svc := &ELBv2ServiceImpl{
		GatewayURL:      "https://192.168.1.33:9999",
		SystemAccessKey: "AKIAIOSFODNN7EXAMPLE",
		SystemSecretKey: "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY",
		region:          "ap-southeast-2",
	}
	ud, err := svc.lbVMUserData("lb-abc123", SchemeInternetFacing)
	require.NoError(t, err)

	envs, runcmd := extractAgentEnv(t, ud)
	assert.Equal(t, map[string]string{
		"LB_LB_ID":       "lb-abc123",
		"LB_GATEWAY_URL": "https://192.168.1.33:9999",
		"LB_ACCESS_KEY":  "AKIAIOSFODNN7EXAMPLE",
		"LB_SECRET_KEY":  "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY",
		"LB_REGION":      "ap-southeast-2",
	}, envs)
	for k := range envs {
		assert.NotContains(t, k, "NATS", "NATS URL must not leak into agent config")
	}

	// Agent is launched via OpenRC, never as a bare binary path. The
	// rc-update line enables the service in the default runlevel so OpenRC
	// auto-starts it on subsequent boots after a host reboot (cloud-init
	// runcmd is PER_INSTANCE and won't re-run).
	require.Len(t, runcmd, 2)
	assert.Contains(t, runcmd[0], `[ "rc-update", "add", "lb-agent", "default" ]`)
	assert.Contains(t, runcmd[1], `[ "rc-service", "lb-agent", "start" ]`)
	assert.NotContains(t, ud, "/usr/local/bin/lb-agent")

	// CA cert is injected upstream by the instance service's template.
	assert.NotContains(t, ud, "ca_certs:")

	// Single-node default: no MgmtRoute set means no bootcmd.
	assert.NotContains(t, ud, "bootcmd:")
}

// TestLbVMUserData_MissingCredentials covers the three required-field error
// paths (gateway URL, access key, secret key) in one table-driven test.
func TestLbVMUserData_MissingCredentials(t *testing.T) {
	tests := []struct {
		name string
		svc  *ELBv2ServiceImpl
	}{
		{
			name: "missing GatewayURL",
			svc:  &ELBv2ServiceImpl{SystemAccessKey: "AKID", SystemSecretKey: "SECRET"},
		},
		{
			name: "missing SystemAccessKey",
			svc:  &ELBv2ServiceImpl{GatewayURL: "https://10.0.0.1:9999", SystemSecretKey: "SECRET"},
		},
		{
			name: "missing SystemSecretKey",
			svc:  &ELBv2ServiceImpl{GatewayURL: "https://10.0.0.1:9999", SystemAccessKey: "AKID"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := tt.svc.lbVMUserData("lb-test", SchemeInternetFacing)
			require.Error(t, err)
			assert.Contains(t, err.Error(), "missing system credentials")
		})
	}
}
