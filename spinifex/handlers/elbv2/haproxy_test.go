package handlers_elbv2

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestGenerateHAProxyConfig_SingleListenerAndBackend(t *testing.T) {
	lb := &LoadBalancerRecord{
		LoadBalancerID: "lb-abc123",
		Name:           "my-alb",
	}

	listeners := []*ListenerRecord{
		{
			ListenerArn:     "arn:aws:elasticloadbalancing:us-east-1:123:listener/app/my-alb/lb-abc123/lst-111",
			LoadBalancerArn: lb.LoadBalancerArn,
			Protocol:        ProtocolHTTP,
			Port:            80,
			DefaultActions: []ListenerAction{
				{Type: ActionTypeForward, TargetGroupArn: "arn:aws:elasticloadbalancing:us-east-1:123:targetgroup/my-tg/tg-222"},
			},
		},
	}

	tgByArn := map[string]*TargetGroupRecord{
		"arn:aws:elasticloadbalancing:us-east-1:123:targetgroup/my-tg/tg-222": {
			TargetGroupArn: "arn:aws:elasticloadbalancing:us-east-1:123:targetgroup/my-tg/tg-222",
			Name:           "my-tg",
			HealthCheck: HealthCheckConfig{
				Path:               "/health",
				Matcher:            "200",
				IntervalSeconds:    30,
				UnhealthyThreshold: 2,
				HealthyThreshold:   5,
			},
			Targets: []Target{
				{Id: "i-aaa111", Port: 8080, PrivateIP: "10.0.1.10", HealthState: TargetHealthHealthy},
				{Id: "i-bbb222", Port: 8080, PrivateIP: "10.0.1.11", HealthState: TargetHealthHealthy},
			},
		},
	}

	config, err := GenerateHAProxyConfig(lb, listeners, tgByArn, nil, "10.0.1.5")
	require.NoError(t, err)

	// Verify global section
	assert.Contains(t, config, "stats socket")
	assert.Contains(t, config, "lb-abc123.sock")

	// Verify frontend
	assert.Contains(t, config, "bind *:80")
	assert.Contains(t, config, "default_backend")

	// Verify backend
	assert.Contains(t, config, "option httpchk GET /health")
	assert.Contains(t, config, "http-check expect status 200")
	assert.Contains(t, config, "10.0.1.10:8080")
	assert.Contains(t, config, "10.0.1.11:8080")
	assert.Contains(t, config, "check inter 30s fall 2 rise 5")
}

func TestGenerateHAProxyConfig_MultipleListeners(t *testing.T) {
	lb := &LoadBalancerRecord{LoadBalancerID: "lb-multi"}

	tgArn1 := "arn:tg1"
	tgArn2 := "arn:tg2"

	listeners := []*ListenerRecord{
		{
			ListenerArn: "arn:lst1",
			Port:        80,
			DefaultActions: []ListenerAction{
				{Type: ActionTypeForward, TargetGroupArn: tgArn1},
			},
		},
		{
			ListenerArn: "arn:lst2",
			Port:        8080,
			DefaultActions: []ListenerAction{
				{Type: ActionTypeForward, TargetGroupArn: tgArn2},
			},
		},
	}

	tgByArn := map[string]*TargetGroupRecord{
		tgArn1: {
			HealthCheck: DefaultHealthCheck(),
			Targets:     []Target{{Id: "i-1", Port: 80, PrivateIP: "10.0.0.1", HealthState: TargetHealthHealthy}},
		},
		tgArn2: {
			HealthCheck: DefaultHealthCheck(),
			Targets:     []Target{{Id: "i-2", Port: 9090, PrivateIP: "10.0.0.2", HealthState: TargetHealthHealthy}},
		},
	}

	config, err := GenerateHAProxyConfig(lb, listeners, tgByArn, nil, "0.0.0.0")
	require.NoError(t, err)

	assert.Contains(t, config, "bind *:80")
	assert.Contains(t, config, "bind *:8080")
	assert.Contains(t, config, "10.0.0.1:80")
	assert.Contains(t, config, "10.0.0.2:9090")
}

func TestGenerateHAProxyConfig_SkipsDrainingTargets(t *testing.T) {
	lb := &LoadBalancerRecord{LoadBalancerID: "lb-drain"}
	tgArn := "arn:tg-drain"

	listeners := []*ListenerRecord{
		{
			ListenerArn:    "arn:lst-drain",
			Port:           80,
			DefaultActions: []ListenerAction{{Type: ActionTypeForward, TargetGroupArn: tgArn}},
		},
	}

	tgByArn := map[string]*TargetGroupRecord{
		tgArn: {
			HealthCheck: DefaultHealthCheck(),
			Targets: []Target{
				{Id: "i-active", Port: 80, PrivateIP: "10.0.0.1", HealthState: TargetHealthHealthy},
				{Id: "i-draining", Port: 80, PrivateIP: "10.0.0.2", HealthState: TargetHealthDraining},
				{Id: "i-no-ip", Port: 80, PrivateIP: "", HealthState: TargetHealthInitial},
			},
		},
	}

	config, err := GenerateHAProxyConfig(lb, listeners, tgByArn, nil, "0.0.0.0")
	require.NoError(t, err)

	assert.Contains(t, config, "10.0.0.1:80")
	assert.NotContains(t, config, "10.0.0.2") // draining
	assert.NotContains(t, config, "i-no-ip")  // no IP
}

func TestGenerateHAProxyConfig_SharedTargetGroup(t *testing.T) {
	lb := &LoadBalancerRecord{LoadBalancerID: "lb-shared"}
	tgArn := "arn:shared-tg"

	// Two listeners pointing to same TG
	listeners := []*ListenerRecord{
		{ListenerArn: "arn:lst-a", Port: 80, DefaultActions: []ListenerAction{{Type: ActionTypeForward, TargetGroupArn: tgArn}}},
		{ListenerArn: "arn:lst-b", Port: 443, DefaultActions: []ListenerAction{{Type: ActionTypeForward, TargetGroupArn: tgArn}}},
	}

	tgByArn := map[string]*TargetGroupRecord{
		tgArn: {
			HealthCheck: DefaultHealthCheck(),
			Targets:     []Target{{Id: "i-1", Port: 80, PrivateIP: "10.0.0.1", HealthState: TargetHealthHealthy}},
		},
	}

	config, err := GenerateHAProxyConfig(lb, listeners, tgByArn, nil, "0.0.0.0")
	require.NoError(t, err)

	// Backend should only appear once
	assert.Equal(t, 1, strings.Count(config, "balance roundrobin"))
}

// TestGenerateHAProxyConfig_FixedResponseDefault mirrors the shared-ingress
// shape: a listener whose default action is fixed-response (no target group),
// with host-header rules forwarding to per-app target groups. The generator
// must synthesize a default backend instead of emitting a dangling
// `default_backend bk_`, which makes HAProxy fail to start.
func TestGenerateHAProxyConfig_FixedResponseDefault(t *testing.T) {
	lb := &LoadBalancerRecord{LoadBalancerID: "lb-ingress"}
	listenerArn := "arn:aws:elasticloadbalancing:ap-southeast-2:1:listener/app/wd-ingress/lb-ingress/lst-616760baf6a3031c3"
	appTG := "arn:aws:elasticloadbalancing:ap-southeast-2:1:targetgroup/wd-identity/tg-app1"

	listeners := []*ListenerRecord{
		{
			ListenerArn: listenerArn,
			Port:        80,
			Protocol:    ProtocolHTTP,
			DefaultActions: []ListenerAction{
				{Type: ActionTypeFixedResponse, FixedResponse: &FixedResponseAction{
					StatusCode:  "404",
					ContentType: "text/plain",
					MessageBody: "no route",
				}},
			},
		},
	}

	rulesByListener := map[string][]*RuleRecord{
		listenerArn: {
			{
				RuleID:      "rule1",
				ListenerArn: listenerArn,
				Priority:    1,
				Conditions:  []RuleCondition{{Field: RuleFieldHostHeader, Values: []string{"identity.toc.spinifex.local"}}},
				Actions:     []ListenerAction{{Type: ActionTypeForward, TargetGroupArn: appTG}},
			},
		},
	}

	tgByArn := map[string]*TargetGroupRecord{
		appTG: {
			TargetGroupArn: appTG,
			HealthCheck:    DefaultHealthCheck(),
			Targets:        []Target{{Id: "i-app1", Port: 8080, PrivateIP: "10.42.0.9", HealthState: TargetHealthHealthy}},
		},
	}

	config, err := GenerateHAProxyConfig(lb, listeners, tgByArn, rulesByListener, "10.42.0.13")
	require.NoError(t, err)

	// No dangling default backend.
	assert.NotContains(t, config, "default_backend bk_\n")
	// Synthetic default backend named off the listener ARN, returning the fixed response.
	assert.Contains(t, config, "default_backend bkdefault_lst-616760baf6a3031c3")
	assert.Contains(t, config, "backend bkdefault_lst-616760baf6a3031c3")
	assert.Contains(t, config, `http-request return status 404 content-type "text/plain" string "no route"`)
	// Host-header rule still routes to the app backend.
	assert.Contains(t, config, "use_backend bk_tg-app1 if")
	assert.Contains(t, config, "10.42.0.9:8080")
}

// TestGenerateHAProxyConfig_FixedResponseRejectsUnsafeBody falls back to a bare
// status when the body or content-type contains injection bytes.
func TestGenerateHAProxyConfig_FixedResponseRejectsUnsafeBody(t *testing.T) {
	lb := &LoadBalancerRecord{LoadBalancerID: "lb-evil"}
	listeners := []*ListenerRecord{
		{
			ListenerArn: "arn:lst-evil",
			Port:        80,
			DefaultActions: []ListenerAction{
				{Type: ActionTypeFixedResponse, FixedResponse: &FixedResponseAction{
					StatusCode:  "200",
					ContentType: "text/plain",
					MessageBody: "evil\"\n  acl x always_true",
				}},
			},
		},
	}

	config, err := GenerateHAProxyConfig(lb, listeners, nil, nil, "0.0.0.0")
	require.NoError(t, err)

	assert.Contains(t, config, "http-request return status 200")
	assert.NotContains(t, config, "always_true")
	assert.NotContains(t, config, `string "evil`)
}

// TestGenerateHAProxyConfig_RedirectSchemeDefault renders the canonical
// HTTP→HTTPS redirect (scheme-only change, all other fields default) as a
// `redirect scheme https` directive so HAProxy preserves host/path/query.
func TestGenerateHAProxyConfig_RedirectSchemeDefault(t *testing.T) {
	lb := &LoadBalancerRecord{LoadBalancerID: "lb-redir"}
	listenerArn := "arn:aws:elasticloadbalancing:ap-southeast-2:1:listener/app/r/lb-redir/lst-aa"
	listeners := []*ListenerRecord{
		{
			ListenerArn: listenerArn,
			Port:        80,
			Protocol:    ProtocolHTTP,
			DefaultActions: []ListenerAction{
				{Type: ActionTypeRedirect, Redirect: &RedirectAction{
					Protocol:   "HTTPS",
					Host:       "#{host}",
					Path:       "/#{path}",
					Query:      "#{query}",
					StatusCode: "HTTP_301",
				}},
			},
		},
	}

	config, err := GenerateHAProxyConfig(lb, listeners, nil, nil, "0.0.0.0")
	require.NoError(t, err)

	assert.Contains(t, config, "default_backend bkdefault_lst-aa")
	assert.Contains(t, config, "http-request redirect scheme https code 301")
	assert.NotContains(t, config, "location")
}

// TestGenerateHAProxyConfig_RedirectLocation renders a redirect that changes
// the host as a rebuilt `location` with HAProxy placeholders preserved.
func TestGenerateHAProxyConfig_RedirectLocation(t *testing.T) {
	lb := &LoadBalancerRecord{LoadBalancerID: "lb-redir2"}
	listenerArn := "arn:aws:elasticloadbalancing:ap-southeast-2:1:listener/app/r/lb-redir2/lst-bb"
	listeners := []*ListenerRecord{
		{
			ListenerArn: listenerArn,
			Port:        80,
			Protocol:    ProtocolHTTP,
			DefaultActions: []ListenerAction{
				{Type: ActionTypeRedirect, Redirect: &RedirectAction{
					Protocol:   "HTTPS",
					Host:       "www.example.com",
					Path:       "/#{path}",
					Query:      "#{query}",
					StatusCode: "HTTP_302",
				}},
			},
		},
	}

	config, err := GenerateHAProxyConfig(lb, listeners, nil, nil, "0.0.0.0")
	require.NoError(t, err)

	assert.Contains(t, config, "http-request redirect location https://www.example.com%[path]?%[query] code 302")
}

// TestGenerateHAProxyConfig_RedirectCustomAll exercises the location form with
// a placeholder protocol (resolved from the listener), an explicit port, and a
// custom literal path + query.
func TestGenerateHAProxyConfig_RedirectCustomAll(t *testing.T) {
	lb := &LoadBalancerRecord{LoadBalancerID: "lb-redir3"}
	listenerArn := "arn:aws:elasticloadbalancing:ap-southeast-2:1:listener/app/r/lb-redir3/lst-dd"
	listeners := []*ListenerRecord{
		{
			ListenerArn: listenerArn,
			Port:        80,
			Protocol:    ProtocolHTTPS,
			DefaultActions: []ListenerAction{
				{Type: ActionTypeRedirect, Redirect: &RedirectAction{
					Protocol:   "#{protocol}",
					Host:       "alt.example.com",
					Port:       "8443",
					Path:       "/moved",
					Query:      "ref=1",
					StatusCode: "HTTP_302",
				}},
			},
		},
	}

	config, err := GenerateHAProxyConfig(lb, listeners, nil, nil, "0.0.0.0")
	require.NoError(t, err)

	assert.Contains(t, config, "http-request redirect location https://alt.example.com:8443/moved?ref=1 code 302")
}

// TestGenerateHAProxyConfig_RedirectRule renders a per-rule redirect action via
// a synthetic backend reached by use_backend.
func TestGenerateHAProxyConfig_RedirectRule(t *testing.T) {
	lb := &LoadBalancerRecord{LoadBalancerID: "lb-rr"}
	listenerArn := "arn:aws:elasticloadbalancing:ap-southeast-2:1:listener/app/r/lb-rr/lst-cc"
	appTG := "arn:aws:elasticloadbalancing:ap-southeast-2:1:targetgroup/app/tg-app"
	listeners := []*ListenerRecord{
		{
			ListenerArn: listenerArn,
			Port:        80,
			Protocol:    ProtocolHTTP,
			DefaultActions: []ListenerAction{
				{Type: ActionTypeForward, TargetGroupArn: appTG},
			},
		},
	}
	rulesByListener := map[string][]*RuleRecord{
		listenerArn: {
			{
				RuleID:      "ruleR",
				ListenerArn: listenerArn,
				Priority:    1,
				Conditions:  []RuleCondition{{Field: RuleFieldPathPattern, Values: []string{"/old/*"}}},
				Actions: []ListenerAction{{Type: ActionTypeRedirect, Redirect: &RedirectAction{
					Protocol:   "HTTPS",
					StatusCode: "HTTP_301",
				}}},
			},
		},
	}
	tgByArn := map[string]*TargetGroupRecord{
		appTG: {TargetGroupArn: appTG, HealthCheck: DefaultHealthCheck()},
	}

	config, err := GenerateHAProxyConfig(lb, listeners, tgByArn, rulesByListener, "0.0.0.0")
	require.NoError(t, err)

	assert.Contains(t, config, "use_backend bkrule_ruleR if")
	assert.Contains(t, config, "backend bkrule_ruleR")
	assert.Contains(t, config, "http-request redirect scheme https code 301")
}

// TestGenerateHAProxyConfig_RedirectRejectsInjection fails the render when a
// redirect field carries HAProxy meta-characters that survived to the store.
func TestGenerateHAProxyConfig_RedirectRejectsInjection(t *testing.T) {
	lb := &LoadBalancerRecord{LoadBalancerID: "lb-evil2"}
	listeners := []*ListenerRecord{
		{
			ListenerArn: "arn:lst-evil2",
			Port:        80,
			Protocol:    ProtocolHTTP,
			DefaultActions: []ListenerAction{
				{Type: ActionTypeRedirect, Redirect: &RedirectAction{
					Host:       "evil\"\n  acl x always_true",
					StatusCode: "HTTP_301",
				}},
			},
		},
	}

	_, err := GenerateHAProxyConfig(lb, listeners, nil, nil, "0.0.0.0")
	require.Error(t, err)
}

func TestGenerateHAProxyConfig_NoListeners(t *testing.T) {
	lb := &LoadBalancerRecord{LoadBalancerID: "lb-empty"}
	config, err := GenerateHAProxyConfig(lb, nil, nil, nil, "0.0.0.0")
	require.NoError(t, err)

	// Should still produce a valid config (global + defaults, no frontends/backends)
	assert.Contains(t, config, "global")
	assert.Contains(t, config, "defaults")
	assert.NotContains(t, config, "frontend")
	assert.NotContains(t, config, "backend")
}

func TestSanitizeName(t *testing.T) {
	tests := []struct {
		prefix string
		input  string
		want   string
	}{
		{"bk", "tg-abc123", "bk_tg-abc123"},
		{"ft", "arn:aws:elasticloadbalancing:us-east-1:123:listener/app/my-alb/lb-123/lst-456", "bk" + ""},
		{"srv", "i-abcdef123", "srv_i-abcdef123"},
	}

	// Just verify no panics and reasonable output
	for _, tc := range tests {
		result := sanitizeName(tc.prefix, tc.input)
		assert.True(t, strings.HasPrefix(result, tc.prefix+"_"))
		assert.NotContains(t, result, ":")
		assert.NotContains(t, result, "/")
	}
}

func TestHAProxyManager_Available_NotFound(t *testing.T) {
	mgr := NewHAProxyManager(t.TempDir())
	mgr.binPath = "/nonexistent/haproxy-fake-bin"
	assert.False(t, mgr.Available())
}

func TestHAProxyManager_IsRunning_UnknownLB(t *testing.T) {
	mgr := NewHAProxyManager(t.TempDir())
	assert.False(t, mgr.IsRunning("lb-unknown"))
}

func TestHAProxyManager_Stop_NotRunning(t *testing.T) {
	mgr := NewHAProxyManager(t.TempDir())
	err := mgr.Stop("lb-not-running")
	require.NoError(t, err)
}

func TestHAProxyManager_StopAll_Empty(t *testing.T) {
	mgr := NewHAProxyManager(t.TempDir())
	mgr.StopAll() // should not panic
}

func TestHAProxyManager_PidFilePath(t *testing.T) {
	mgr := NewHAProxyManager("/tmp/haproxy-test")
	assert.Equal(t, "/tmp/haproxy-test/lb-123.pid", mgr.pidFilePath("lb-123"))
	assert.Equal(t, "/tmp/haproxy-test/lb-123.cfg", mgr.configFilePath("lb-123"))
}

func TestHAProxyManager_Start_MissingBinary(t *testing.T) {
	dir := t.TempDir()
	mgr := NewHAProxyManager(dir)
	mgr.binPath = "/nonexistent/haproxy"

	// Write a config so the path exists
	_, err := mgr.WriteConfig("lb-test", "global\n")
	require.NoError(t, err)

	err = mgr.Start("lb-test")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "haproxy start failed")
}

func TestHAProxyManager_Reload_NotRunning_FallsBackToStart(t *testing.T) {
	dir := t.TempDir()
	mgr := NewHAProxyManager(dir)
	mgr.binPath = "/nonexistent/haproxy"

	// Write a config so the path exists
	_, err := mgr.WriteConfig("lb-test", "global\n")
	require.NoError(t, err)

	// Reload when not running should attempt Start (which will fail due to bad binary)
	err = mgr.Reload("lb-test")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "haproxy start failed")
}

func TestHAProxyManager_LifecycleWithFakeHAProxy(t *testing.T) {
	// Create a fake haproxy script that writes its PID to the pidfile
	dir := t.TempDir()
	fakeHAProxy := filepath.Join(dir, "fake-haproxy")
	script := `#!/bin/sh
# Parse -p flag for pidfile
PIDFILE=""
SF_PID=""
while [ $# -gt 0 ]; do
    case "$1" in
        -p) PIDFILE="$2"; shift 2;;
        -D) shift;;
        -f) shift 2;;
        -c) exit 0;;
        -sf) SF_PID="$2"; shift 2;;
        *) shift;;
    esac
done
# Kill old process if -sf was passed
if [ -n "$SF_PID" ]; then
    kill "$SF_PID" 2>/dev/null || true
fi
# Start a background sleep with closed fds so parent can exit
sleep 60 </dev/null >/dev/null 2>&1 &
BGPID=$!
if [ -n "$PIDFILE" ]; then
    echo "$BGPID" > "$PIDFILE"
fi
`
	require.NoError(t, os.WriteFile(fakeHAProxy, []byte(script), 0o755))

	configDir := filepath.Join(dir, "configs")
	mgr := NewHAProxyManager(configDir)
	mgr.binPath = fakeHAProxy

	lbID := "lb-lifecycle-test"

	// Write config
	content := "global\n  log stdout\n"
	_, err := mgr.WriteConfig(lbID, content)
	require.NoError(t, err)

	// Not running yet
	assert.False(t, mgr.IsRunning(lbID))

	// Start
	err = mgr.Start(lbID)
	require.NoError(t, err)
	assert.True(t, mgr.IsRunning(lbID))

	// Reload
	err = mgr.Reload(lbID)
	require.NoError(t, err)
	assert.True(t, mgr.IsRunning(lbID))

	// Stop
	err = mgr.Stop(lbID)
	require.NoError(t, err)
	assert.False(t, mgr.IsRunning(lbID))

	// Pidfile should be cleaned up
	_, err = os.Stat(mgr.pidFilePath(lbID))
	assert.True(t, os.IsNotExist(err))
}

func TestHAProxyManager_WriteAndRemoveConfig(t *testing.T) {
	dir := t.TempDir()
	mgr := &HAProxyManager{configDir: dir, binPath: "/usr/sbin/haproxy"}

	content := "global\n  log stdout\n"
	path, err := mgr.WriteConfig("lb-test", content)
	require.NoError(t, err)
	assert.Equal(t, filepath.Join(dir, "lb-test.cfg"), path)

	// Verify file contents
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, content, string(data))

	// Remove
	err = mgr.RemoveConfig("lb-test")
	require.NoError(t, err)

	_, err = os.Stat(path)
	assert.True(t, os.IsNotExist(err))
}

func TestHAProxyManager_RemoveConfig_Idempotent(t *testing.T) {
	dir := t.TempDir()
	mgr := &HAProxyManager{configDir: dir, binPath: "/usr/sbin/haproxy"}

	err := mgr.RemoveConfig("nonexistent")
	require.NoError(t, err)
}

func TestGenerateALBConfig_StillWorks(t *testing.T) {
	// Regression: ALB (no Type set) should still generate HTTP-mode config
	lb := &LoadBalancerRecord{
		LoadBalancerID: "lb-alb-regression",
		Name:           "alb-regression",
		Type:           LoadBalancerTypeApplication,
	}

	tgArn := "arn:tg-alb-reg"
	listeners := []*ListenerRecord{
		{
			ListenerArn: "arn:lst-alb-reg",
			Protocol:    ProtocolHTTP,
			Port:        80,
			DefaultActions: []ListenerAction{
				{Type: ActionTypeForward, TargetGroupArn: tgArn},
			},
		},
	}

	tgByArn := map[string]*TargetGroupRecord{
		tgArn: {
			HealthCheck: HealthCheckConfig{
				Protocol:           ProtocolHTTP,
				Path:               "/health",
				Matcher:            "200",
				IntervalSeconds:    30,
				HealthyThreshold:   5,
				UnhealthyThreshold: 2,
			},
			Targets: []Target{
				{Id: "i-alb1", Port: 80, PrivateIP: "10.0.0.1", HealthState: TargetHealthHealthy},
			},
		},
	}

	config, err := GenerateHAProxyConfig(lb, listeners, tgByArn, nil, "0.0.0.0")
	require.NoError(t, err)

	assert.Contains(t, config, "mode http")
	assert.Contains(t, config, "option httplog")
	assert.Contains(t, config, "option httpchk GET /health")
	assert.Contains(t, config, "http-check expect status 200")
	assert.NotContains(t, config, "mode tcp")
	assert.NotContains(t, config, "option tcplog")
}
