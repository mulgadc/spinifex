package handlers_elbv2

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func nlbHealthCheck() HealthCheckConfig {
	return HealthCheckConfig{
		Protocol:           ProtocolTCP,
		Port:               "traffic-port",
		IntervalSeconds:    30,
		TimeoutSeconds:     10,
		HealthyThreshold:   3,
		UnhealthyThreshold: 3,
	}
}

func tcpListener(arn, listenerID, protocol string, port int64, tgArn string) *ListenerRecord {
	return &ListenerRecord{
		ListenerArn: arn,
		ListenerID:  listenerID,
		Protocol:    protocol,
		Port:        port,
		DefaultActions: []ListenerAction{
			{Type: ActionTypeForward, TargetGroupArn: tgArn},
		},
	}
}

func nlbTG(arn string, targets ...Target) *TargetGroupRecord {
	return &TargetGroupRecord{
		TargetGroupArn: arn,
		Protocol:       ProtocolTCP,
		HealthCheck:    nlbHealthCheck(),
		Targets:        targets,
	}
}

func TestGenerateNLBStream_TCP(t *testing.T) {
	lb := &LoadBalancerRecord{LoadBalancerID: "lb-tcp", Type: LoadBalancerTypeNetwork}
	tgArn := "arn:aws:elasticloadbalancing:us-east-1:1:targetgroup/db/tg-db"
	listeners := []*ListenerRecord{tcpListener("arn:lst-tcp", "lst-tcp", ProtocolTCP, 5432, tgArn)}
	tgByArn := map[string]*TargetGroupRecord{
		tgArn: nlbTG(tgArn,
			Target{Id: "i-1", Port: 5432, PrivateIP: "10.0.1.10", HealthState: TargetHealthHealthy},
			Target{Id: "i-2", Port: 5432, PrivateIP: "10.0.1.11", HealthState: TargetHealthHealthy},
		),
	}

	config, certs, err := GenerateHAProxyConfigWithCerts(lb, listeners, tgByArn, nil, "10.0.1.5", nil)
	require.NoError(t, err)
	assert.Empty(t, certs)

	assert.Contains(t, config, "load_module /usr/lib/nginx/modules/ngx_stream_module.so;") // stream is a separate Alpine module
	assert.Contains(t, config, "stream {")
	assert.NotContains(t, config, "mode tcp") // not HAProxy
	assert.Contains(t, config, "listen 5432;")
	assert.NotContains(t, config, "listen 5432 udp")
	assert.Contains(t, config, "server 10.0.1.10:5432 max_fails=3 fail_timeout=30s;")
	assert.Contains(t, config, "server 10.0.1.11:5432 max_fails=3 fail_timeout=30s;")
	assert.Contains(t, config, "proxy_pass us_tg-db;")
}

func TestGenerateNLBStream_UDP(t *testing.T) {
	lb := &LoadBalancerRecord{LoadBalancerID: "lb-udp", Type: LoadBalancerTypeNetwork}
	tgArn := "arn:tg-dns"
	listeners := []*ListenerRecord{tcpListener("arn:lst-udp", "lst-udp", ProtocolUDP, 53, tgArn)}
	tgByArn := map[string]*TargetGroupRecord{
		tgArn: nlbTG(tgArn, Target{Id: "i-dns", Port: 53, PrivateIP: "10.0.2.10", HealthState: TargetHealthHealthy}),
	}

	config, _, err := GenerateHAProxyConfigWithCerts(lb, listeners, tgByArn, nil, "10.0.2.1", nil)
	require.NoError(t, err)

	assert.Contains(t, config, "listen 53 udp;")
	assert.Contains(t, config, "proxy_responses 1;")
	assert.Contains(t, config, "10.0.2.10:53")
}

func TestGenerateNLBStream_TLS(t *testing.T) {
	lb := &LoadBalancerRecord{LoadBalancerID: "lb-tls", Type: LoadBalancerTypeNetwork}
	tgArn := "arn:tg-tls"
	lst := tcpListener("arn:lst-tls", "lst-tls", ProtocolTLS, 443, tgArn)
	lst.Certificates = []ListenerCertificate{{CertificateArn: "arn:cert-1", IsDefault: true}}
	tgByArn := map[string]*TargetGroupRecord{
		tgArn: nlbTG(tgArn, Target{Id: "i-tls", Port: 8443, PrivateIP: "10.0.3.10", HealthState: TargetHealthHealthy}),
	}
	certPEM := map[string]string{"arn:cert-1": "LEAF\nCHAIN\nKEY\n"}

	config, certs, err := GenerateHAProxyConfigWithCerts(lb, []*ListenerRecord{lst}, tgByArn, nil, "10.0.3.1", certPEM)
	require.NoError(t, err)

	wantPath := "/etc/nginx/certs/lb-tls-lst-tls.pem"
	assert.Contains(t, config, "listen 443 ssl;")
	assert.Contains(t, config, "ssl_certificate "+wantPath+";")
	assert.Contains(t, config, "ssl_certificate_key "+wantPath+";")
	require.Contains(t, certs, wantPath)
	assert.Equal(t, "LEAF\nCHAIN\nKEY\n", certs[wantPath])
}

func TestGenerateNLBStream_TCPUDP(t *testing.T) {
	lb := &LoadBalancerRecord{LoadBalancerID: "lb-both", Type: LoadBalancerTypeNetwork}
	tgArn := "arn:tg-both"
	listeners := []*ListenerRecord{tcpListener("arn:lst-both", "lst-both", ProtocolTCPUDP, 5000, tgArn)}
	tgByArn := map[string]*TargetGroupRecord{
		tgArn: nlbTG(tgArn, Target{Id: "i-both", Port: 5000, PrivateIP: "10.0.4.10", HealthState: TargetHealthHealthy}),
	}

	config, _, err := GenerateHAProxyConfigWithCerts(lb, listeners, tgByArn, nil, "10.0.4.1", nil)
	require.NoError(t, err)

	// Two server blocks on the same port: one TCP, one UDP.
	assert.Contains(t, config, "listen 5000;")
	assert.Contains(t, config, "listen 5000 udp;")
	assert.Equal(t, 2, strings.Count(config, "listen 5000"))
}

func TestGenerateNLBStream_SkipsDrainingAndNoIP(t *testing.T) {
	lb := &LoadBalancerRecord{LoadBalancerID: "lb-drain", Type: LoadBalancerTypeNetwork}
	tgArn := "arn:tg-drain"
	listeners := []*ListenerRecord{tcpListener("arn:lst-drain", "lst-drain", ProtocolTCP, 3306, tgArn)}
	tgByArn := map[string]*TargetGroupRecord{
		tgArn: nlbTG(tgArn,
			Target{Id: "i-active", Port: 3306, PrivateIP: "10.0.0.1", HealthState: TargetHealthHealthy},
			Target{Id: "i-drain", Port: 3306, PrivateIP: "10.0.0.2", HealthState: TargetHealthDraining},
			Target{Id: "i-no-ip", Port: 3306, PrivateIP: "", HealthState: TargetHealthInitial},
		),
	}

	config, _, err := GenerateHAProxyConfigWithCerts(lb, listeners, tgByArn, nil, "0.0.0.0", nil)
	require.NoError(t, err)

	assert.Contains(t, config, "10.0.0.1:3306")
	assert.NotContains(t, config, "10.0.0.2")
}

func TestGenerateNLBStream_EmptyUpstreamPlaceholder(t *testing.T) {
	lb := &LoadBalancerRecord{LoadBalancerID: "lb-empty", Type: LoadBalancerTypeNetwork}
	tgArn := "arn:tg-empty"
	listeners := []*ListenerRecord{tcpListener("arn:lst-empty", "lst-empty", ProtocolTCP, 80, tgArn)}
	tgByArn := map[string]*TargetGroupRecord{tgArn: nlbTG(tgArn)}

	config, _, err := GenerateHAProxyConfigWithCerts(lb, listeners, tgByArn, nil, "0.0.0.0", nil)
	require.NoError(t, err)

	// nginx requires >=1 server per upstream; an empty TG renders a down placeholder.
	assert.Contains(t, config, "server 127.0.0.1:1 down;")
}

func TestBuildNLBHealthTargets_Resolution(t *testing.T) {
	tgArn := "arn:aws:elasticloadbalancing:us-east-1:1:targetgroup/db/tg-db"
	tgByArn := map[string]*TargetGroupRecord{
		tgArn: nlbTG(tgArn,
			Target{Id: "i-2", Port: 5432, PrivateIP: "10.0.1.11", HealthState: TargetHealthHealthy},
			Target{Id: "i-1", Port: 5432, PrivateIP: "10.0.1.10", HealthState: TargetHealthHealthy},
			Target{Id: "i-drain", Port: 5432, PrivateIP: "10.0.1.12", HealthState: TargetHealthDraining},
			Target{Id: "i-no-ip", Port: 5432, PrivateIP: "", HealthState: TargetHealthInitial},
		),
	}

	got := buildNLBHealthTargets(tgByArn)
	require.Len(t, got, 2) // draining + no-IP excluded

	// Sorted by server name for deterministic delivery.
	assert.Equal(t, sanitizeName("srv", "i-1"), got[0].ServerName)
	assert.Equal(t, sanitizeName("srv", "i-2"), got[1].ServerName)
	assert.Equal(t, "10.0.1.10:5432", got[0].Address) // traffic-port -> target port
	assert.Equal(t, ProtocolTCP, got[0].Protocol)
}

func TestBuildNLBHealthTargets_ExplicitHCPortAndHTTP(t *testing.T) {
	tgArn := "arn:tg-http"
	tg := nlbTG(tgArn, Target{Id: "i-1", Port: 8080, PrivateIP: "10.0.0.9", HealthState: TargetHealthHealthy})
	tg.HealthCheck.Protocol = ProtocolHTTP
	tg.HealthCheck.Port = "9000"
	tg.HealthCheck.Path = "/healthz"
	tgByArn := map[string]*TargetGroupRecord{tgArn: tg}

	got := buildNLBHealthTargets(tgByArn)
	require.Len(t, got, 1)
	assert.Equal(t, "10.0.0.9:9000", got[0].Address) // explicit numeric HC port wins
	assert.Equal(t, ProtocolHTTP, got[0].Protocol)
	assert.Equal(t, "/healthz", got[0].Path)
}
