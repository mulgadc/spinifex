package elbv2

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func fixedResponseListener(protocol string, port int64) *ListenerRecord {
	return &ListenerRecord{
		ListenerArn:    "arn:aws:elasticloadbalancing:us-east-1:123:listener/app/my-alb/lb-abc123/lst-fwd",
		ListenerID:     "lst-fwd",
		Protocol:       protocol,
		Port:           port,
		DefaultActions: []ListenerAction{{Type: ActionTypeFixedResponse, FixedResponse: &FixedResponseAction{StatusCode: "200"}}},
	}
}

func renderForwarding(t *testing.T, attrs map[string]string, l *ListenerRecord) string {
	t.Helper()
	lb := &LoadBalancerRecord{LoadBalancerID: "lb-abc123", Name: "my-alb", Attributes: attrs}
	certs := map[string]string{certTestArn: "CERTPEM\nKEYPEM\n"}
	cfg, _, err := GenerateHAProxyConfigWithCerts(lb, []*ListenerRecord{l}, nil, nil, "0.0.0.0", certs)
	require.NoError(t, err)
	return cfg
}

func TestForwardedHeaders_DefaultsAppendWithoutPort(t *testing.T) {
	t.Parallel()
	cfg := renderForwarding(t, nil, fixedResponseListener(ProtocolHTTP, 80))

	assert.Contains(t, cfg, `http-request set-header X-Forwarded-For "%[var(txn.xff)], %[src]" if { var(txn.xff) -m len gt 0 }`)
	assert.Contains(t, cfg, `http-request set-header X-Forwarded-For "%[src]" unless { var(txn.xff) -m len gt 0 }`)
	assert.Contains(t, cfg, "http-request set-header X-Forwarded-Proto http\n")
	assert.Contains(t, cfg, "http-request set-header X-Forwarded-Port 80\n")
	assert.NotContains(t, cfg, "src_port")
	assert.NotContains(t, cfg, "del-header")
}

func TestForwardedHeaders_ModeAndClientPort(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		attrs      map[string]string
		wantAll    []string
		wantAbsent []string
	}{
		{
			name:  "append with client port",
			attrs: map[string]string{AttrXFFHeaderProcessing: "append", AttrXFFClientPort: "true"},
			wantAll: []string{
				`"[%[src]]:%[src_port]" if { src -m reg : }`,
				`"%[src]:%[src_port]" unless { src -m reg : }`,
				`set-header X-Forwarded-For "%[var(txn.xff)], %[var(txn.xffc)]"`,
				`set-header X-Forwarded-For "%[var(txn.xffc)]" unless`,
			},
			wantAbsent: []string{"del-header"},
		},
		{
			name:       "append without client port",
			attrs:      map[string]string{AttrXFFHeaderProcessing: "append", AttrXFFClientPort: "false"},
			wantAll:    []string{`set-header X-Forwarded-For "%[src]" unless`},
			wantAbsent: []string{"src_port", "txn.xffc"},
		},
		{
			name:       "preserve leaves the header alone",
			attrs:      map[string]string{AttrXFFHeaderProcessing: "preserve"},
			wantAll:    []string{"set-header X-Forwarded-Proto", "set-header X-Forwarded-Port"},
			wantAbsent: []string{"set-header X-Forwarded-For", "del-header", "txn.xff"},
		},
		{
			name:       "preserve ignores client port",
			attrs:      map[string]string{AttrXFFHeaderProcessing: "preserve", AttrXFFClientPort: "true"},
			wantAbsent: []string{"set-header X-Forwarded-For", "src_port"},
		},
		{
			name:       "remove deletes the header",
			attrs:      map[string]string{AttrXFFHeaderProcessing: "remove"},
			wantAll:    []string{"http-request del-header X-Forwarded-For\n", "set-header X-Forwarded-Proto"},
			wantAbsent: []string{"set-header X-Forwarded-For", "txn.xff"},
		},
		{
			name:    "unknown mode renders the default",
			attrs:   map[string]string{AttrXFFHeaderProcessing: "bogus"},
			wantAll: []string{`set-header X-Forwarded-For "%[src]" unless`},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			cfg := renderForwarding(t, tt.attrs, fixedResponseListener(ProtocolHTTP, 8080))
			for _, w := range tt.wantAll {
				assert.Contains(t, cfg, w)
			}
			for _, a := range tt.wantAbsent {
				assert.NotContains(t, cfg, a)
			}
			assert.Contains(t, cfg, "http-request set-header X-Forwarded-Port 8080\n")
		})
	}
}

func TestForwardedHeaders_HTTPSListenerSetsProtoAndPort(t *testing.T) {
	t.Parallel()
	cfg := renderForwarding(t, nil, httpsListener())

	assert.Contains(t, cfg, "http-request set-header X-Forwarded-Proto https\n")
	assert.Contains(t, cfg, "http-request set-header X-Forwarded-Port 443\n")
	assert.NotContains(t, cfg, "X-Forwarded-Proto http\n")
}

func TestForwardedHeaders_RulesPrecedeRouting(t *testing.T) {
	t.Parallel()
	cfg := renderForwarding(t, nil, fixedResponseListener(ProtocolHTTP, 80))

	bind := strings.Index(cfg, "bind *:80")
	rule := strings.Index(cfg, "http-request set-header X-Forwarded-For")
	def := strings.Index(cfg, "default_backend")
	require.True(t, bind >= 0 && rule > bind && def > rule, "bind < header rules < default_backend")
}

func TestForwardedHeaders_AppendReadsEveryLine(t *testing.T) {
	t.Parallel()
	rules := strings.Join(forwardedHeaderRules(XFFModeAppend, false, false, 80), "\n")

	for i := 1; i <= maxXFFLines; i++ {
		assert.Contains(t, rules, fmt.Sprintf("req.fhdr(X-Forwarded-For,%d)", i))
	}
	assert.NotContains(t, rules, "req.hdr(", "req.hdr keeps only the last comma-separated item")
}

const haproxyTestImage = "haproxy:3.2-alpine"

// startHAProxyEcho runs haproxy with the rendered forwarding rules in front of
// a fixed response that echoes the header the target would receive, and
// returns the published address. It skips when the image is not present.
func startHAProxyEcho(t *testing.T, rules []string) string {
	t.Helper()
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker not available")
	}
	if err := exec.Command("docker", "image", "inspect", haproxyTestImage).Run(); err != nil {
		t.Skipf("%s not present locally", haproxyTestImage)
	}

	conf := "global\n    log stdout format short daemon\n" +
		"defaults\n    mode http\n    timeout connect 5s\n    timeout client 20s\n    timeout server 20s\n" +
		"frontend f\n    bind *:8080\n    " + strings.Join(rules, "\n    ") + "\n" +
		"    http-request return status 200 content-type text/plain lf-string \"%[req.fhdr(X-Forwarded-For,-1)]|%[req.fhdr(X-Forwarded-Proto)]|%[req.fhdr(X-Forwarded-Port)]\"\n"
	path := filepath.Join(t.TempDir(), "haproxy.cfg")
	require.NoError(t, os.WriteFile(path, []byte(conf), 0o644))

	out, err := exec.Command("docker", "run", "--rm", "-v", path+":/usr/local/etc/haproxy/haproxy.cfg:ro",
		haproxyTestImage, "haproxy", "-c", "-f", "/usr/local/etc/haproxy/haproxy.cfg").CombinedOutput()
	require.NoError(t, err, "haproxy -c: %s", out)

	id, err := exec.Command("docker", "run", "-d", "--rm", "-p", "127.0.0.1::8080",
		"-v", path+":/usr/local/etc/haproxy/haproxy.cfg:ro", haproxyTestImage).Output()
	require.NoError(t, err)
	cid := strings.TrimSpace(string(id))
	t.Cleanup(func() { _ = exec.Command("docker", "rm", "-f", cid).Run() })

	port, err := exec.Command("docker", "port", cid, "8080/tcp").Output()
	require.NoError(t, err)
	addr := strings.TrimSpace(strings.SplitN(string(port), "\n", 2)[0])
	require.Eventually(t, func() bool {
		resp, err := http.Get("http://" + addr + "/")
		if err != nil {
			return false
		}
		_ = resp.Body.Close()
		return true
	}, 10*time.Second, 200*time.Millisecond)
	return addr
}

func echoXFF(t *testing.T, addr string, xff ...string) (string, string, string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, "http://"+addr+"/", nil)
	require.NoError(t, err)
	for _, v := range xff {
		req.Header.Add("X-Forwarded-For", v)
	}
	req.Header.Set("X-Forwarded-Proto", "spoofed")
	req.Header.Set("X-Forwarded-Port", "1")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	parts := strings.Split(string(body), "|")
	require.Len(t, parts, 3, string(body))
	return parts[0], parts[1], parts[2]
}

func TestForwardedHeaders_HAProxyAppendKeepsEveryClientValue(t *testing.T) {
	addr := startHAProxyEcho(t, forwardedHeaderRules(XFFModeAppend, false, false, 80))
	client := regexp.MustCompile(`\d+\.\d+\.\d+\.\d+$`)

	xff, proto, port := echoXFF(t, addr)
	assert.Regexp(t, `^\d+\.\d+\.\d+\.\d+$`, xff, "no client header: only the client address")
	assert.Equal(t, "http", proto, "client X-Forwarded-Proto is overwritten")
	assert.Equal(t, "80", port, "client X-Forwarded-Port is overwritten")

	xff, _, _ = echoXFF(t, addr, "1.1.1.1, 2.2.2.2")
	assert.True(t, strings.HasPrefix(xff, "1.1.1.1, 2.2.2.2, "), xff)
	assert.Regexp(t, client, xff)

	xff, _, _ = echoXFF(t, addr, "1.1.1.1", "2.2.2.2, 3.3.3.3")
	assert.True(t, strings.HasPrefix(xff, "1.1.1.1, 2.2.2.2, 3.3.3.3, "), "values on earlier lines survive: %s", xff)
}

func TestForwardedHeaders_HAProxyClientPortAndRemove(t *testing.T) {
	addr := startHAProxyEcho(t, forwardedHeaderRules(XFFModeAppend, true, true, 443))
	xff, proto, port := echoXFF(t, addr, "9.9.9.9")
	assert.Regexp(t, `^9\.9\.9\.9, \d+\.\d+\.\d+\.\d+:\d+$`, xff)
	assert.Equal(t, "https", proto)
	assert.Equal(t, "443", port)

	addr = startHAProxyEcho(t, forwardedHeaderRules(XFFModeRemove, false, false, 80))
	xff, _, _ = echoXFF(t, addr, "9.9.9.9")
	assert.Empty(t, xff, "remove drops the client header")

	addr = startHAProxyEcho(t, forwardedHeaderRules(XFFModePreserve, false, false, 80))
	xff, _, _ = echoXFF(t, addr, "9.9.9.9")
	assert.Equal(t, "9.9.9.9", xff, "preserve passes the client header unchanged")
}
