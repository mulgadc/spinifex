package handlers_elbv2

import (
	"encoding/xml"
	"path/filepath"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	handlers_acm "github.com/mulgadc/spinifex/spinifex/handlers/acm"
	"github.com/mulgadc/spinifex/spinifex/lbagent"
	"github.com/mulgadc/spinifex/spinifex/utils"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const certTestArn = "arn:aws:acm:ap-southeast-2:123456789012:certificate/cccc-3333"

func httpsListener() *ListenerRecord {
	return &ListenerRecord{
		ListenerArn:     "arn:aws:elasticloadbalancing:us-east-1:123:listener/app/my-alb/lb-abc123/lst-https",
		ListenerID:      "lst-https",
		Protocol:        ProtocolHTTPS,
		Port:            443,
		Certificates:    []ListenerCertificate{{CertificateArn: certTestArn, IsDefault: true}},
		SslPolicy:       DefaultSslPolicy,
		DefaultActions:  []ListenerAction{{Type: ActionTypeFixedResponse, FixedResponse: &FixedResponseAction{StatusCode: "200"}}},
		LoadBalancerArn: "arn:lb",
	}
}

func TestGenerateHAProxyConfigWithCerts_HTTPSRendersSslCrt(t *testing.T) {
	lb := &LoadBalancerRecord{LoadBalancerID: "lb-abc123", Name: "my-alb"}
	listeners := []*ListenerRecord{httpsListener()}
	certPEM := "CERTPEM\nKEYPEM\n"

	config, certFiles, err := GenerateHAProxyConfigWithCerts(
		lb, listeners, nil, nil, "0.0.0.0",
		map[string]string{certTestArn: certPEM},
	)
	require.NoError(t, err)

	wantPath := filepath.Join(lbagent.CertDir, "lb-abc123-lst-https.pem")
	assert.Contains(t, config, "bind *:443 ssl crt "+wantPath)
	require.Len(t, certFiles, 1)
	assert.Equal(t, certPEM, certFiles[wantPath])
}

func TestGenerateHAProxyConfigWithCerts_HTTPHasNoSslCrt(t *testing.T) {
	lb := &LoadBalancerRecord{LoadBalancerID: "lb-abc123", Name: "my-alb"}
	listeners := []*ListenerRecord{{
		ListenerArn:    "arn:aws:elasticloadbalancing:us-east-1:123:listener/app/my-alb/lb-abc123/lst-http",
		ListenerID:     "lst-http",
		Protocol:       ProtocolHTTP,
		Port:           80,
		DefaultActions: []ListenerAction{{Type: ActionTypeFixedResponse, FixedResponse: &FixedResponseAction{StatusCode: "200"}}},
	}}

	config, certFiles, err := GenerateHAProxyConfigWithCerts(lb, listeners, nil, nil, "0.0.0.0", nil)
	require.NoError(t, err)
	assert.Contains(t, config, "bind *:80")
	assert.NotContains(t, config, "ssl crt")
	assert.Empty(t, certFiles)
}

// A secure listener whose cert ARN is unresolved renders without ssl crt — the
// gap surfaces at HAProxy bind time rather than silently serving cleartext.
func TestGenerateHAProxyConfigWithCerts_MissingCertRendersPlain(t *testing.T) {
	lb := &LoadBalancerRecord{LoadBalancerID: "lb-abc123", Name: "my-alb"}
	config, certFiles, err := GenerateHAProxyConfigWithCerts(
		lb, []*ListenerRecord{httpsListener()}, nil, nil, "0.0.0.0", nil,
	)
	require.NoError(t, err)
	assert.NotContains(t, config, "ssl crt")
	assert.Empty(t, certFiles)
}

func TestConfigCertHash_ChangesOnRotation(t *testing.T) {
	const cfg = "frontend ft\n    bind *:443 ssl crt /etc/haproxy/certs/x.pem\n"
	path := "/etc/haproxy/certs/x.pem"

	base := configCertHash(cfg, map[string]string{path: "PEM-A"})
	rotated := configCertHash(cfg, map[string]string{path: "PEM-B"})
	same := configCertHash(cfg, map[string]string{path: "PEM-A"})

	assert.NotEqual(t, base, rotated, "rotating cert content must change the hash")
	assert.Equal(t, base, same, "identical inputs hash identically")

	// Config-only change also moves the hash.
	assert.NotEqual(t, base, configCertHash(cfg+"\n", map[string]string{path: "PEM-A"}))
}

func putTestCert(t *testing.T, svc *ELBv2ServiceImpl, arn, account, leaf, chain, key string) {
	t.Helper()
	require.NoError(t, svc.acmStore.PutCert(&handlers_acm.CertRecord{
		CertificateArn:   arn,
		AccountID:        account,
		Certificate:      leaf,
		CertificateChain: chain,
		PrivateKey:       key,
		DomainName:       "example.com",
	}))
}

func TestResolveCertPEM_ConcatOrderAndOwnership(t *testing.T) {
	svc := setupTestService(t)
	require.NotNil(t, svc.acmStore)
	putTestCert(t, svc, certTestArn, testAccountID, "LEAF", "CHAIN", "KEY")

	pem, err := svc.resolveCertPEM(certTestArn, testAccountID)
	require.NoError(t, err)
	assert.Equal(t, "LEAF\nCHAIN\nKEY\n", pem)

	// Wrong account → treated as not found.
	_, err = svc.resolveCertPEM(certTestArn, "999999999999")
	require.Error(t, err)

	// Unknown ARN → not found.
	_, err = svc.resolveCertPEM("arn:aws:acm:ap-southeast-2:123456789012:certificate/missing", testAccountID)
	require.Error(t, err)
}

func TestValidateListenerCerts_RejectsUnknownAndWrongAccount(t *testing.T) {
	svc := setupTestService(t)
	putTestCert(t, svc, certTestArn, testAccountID, "LEAF", "", "KEY")

	// Known, owned cert → accepted.
	require.NoError(t, svc.validateListenerCerts(
		[]ListenerCertificate{{CertificateArn: certTestArn}}, testAccountID))

	// Unknown ARN → CertificateNotFound, rejected at the API boundary.
	err := svc.validateListenerCerts(
		[]ListenerCertificate{{CertificateArn: "arn:aws:acm:ap-southeast-2:123456789012:certificate/missing"}}, testAccountID)
	require.EqualError(t, err, awserrors.ErrorELBv2CertificateNotFound)

	// Cert owned by another account → not found.
	err = svc.validateListenerCerts(
		[]ListenerCertificate{{CertificateArn: certTestArn}}, "999999999999")
	require.EqualError(t, err, awserrors.ErrorELBv2CertificateNotFound)

	// No ACM store (degraded) → validation skipped, never blocks.
	svc.acmStore = nil
	require.NoError(t, svc.validateListenerCerts(
		[]ListenerCertificate{{CertificateArn: certTestArn}}, testAccountID))
}

func TestResolveCertPEM_NoChain(t *testing.T) {
	svc := setupTestService(t)
	putTestCert(t, svc, certTestArn, testAccountID, "LEAF", "", "KEY")

	pem, err := svc.resolveCertPEM(certTestArn, testAccountID)
	require.NoError(t, err)
	assert.Equal(t, "LEAF\nKEY\n", pem)
}

func TestResolveListenerCerts_DedupAndResolve(t *testing.T) {
	svc := setupTestService(t)
	putTestCert(t, svc, certTestArn, testAccountID, "LEAF", "", "KEY")

	// Two listeners share the same cert ARN — resolved once.
	got, err := svc.resolveListenerCerts([]*ListenerRecord{httpsListener(), httpsListener()}, testAccountID)
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Equal(t, "LEAF\nKEY\n", got[certTestArn])

	// HTTP-only listeners → nil map, no error.
	got, err = svc.resolveListenerCerts([]*ListenerRecord{{Protocol: ProtocolHTTP}}, testAccountID)
	require.NoError(t, err)
	assert.Nil(t, got)
}

func TestGetLBConfig_DeliversCertFiles(t *testing.T) {
	svc := setupTestService(t)
	rec := &LoadBalancerRecord{
		LoadBalancerID: "lb-deliver1",
		AccountID:      testAccountID,
		ConfigText:     "frontend ft\n    bind *:443 ssl crt /etc/haproxy/certs/lb-deliver1-lst.pem\n",
		ConfigHash:     "hash123",
		CertFiles:      map[string]string{"/etc/haproxy/certs/lb-deliver1-lst.pem": "LEAF\nKEY\n"},
	}
	require.NoError(t, svc.store.PutLoadBalancer(rec))

	out, err := svc.GetLBConfig(&GetLBConfigInput{LBID: aws.String("lb-deliver1")}, testAccountID)
	require.NoError(t, err)
	require.Len(t, out.CertFiles, 1)
	assert.Equal(t, "/etc/haproxy/certs/lb-deliver1-lst.pem", *out.CertFiles[0].Path)
	assert.Equal(t, "LEAF\nKEY\n", *out.CertFiles[0].PEM)

	// The gateway marshals this output to XML; confirm the member shape the
	// lb-agent parses (GetLBConfigResult>CertFiles>member>{Path,PEM}).
	payload := utils.GenerateIAMXMLPayload("GetLBConfig", *out)
	xmlBytes, err := utils.MarshalToXML(payload)
	require.NoError(t, err)
	var parsed struct {
		Members []struct {
			Path string `xml:"Path"`
			PEM  string `xml:"PEM"`
		} `xml:"GetLBConfigResult>CertFiles>member"`
	}
	require.NoError(t, xml.Unmarshal(xmlBytes, &parsed))
	require.Len(t, parsed.Members, 1)
	assert.Equal(t, "/etc/haproxy/certs/lb-deliver1-lst.pem", parsed.Members[0].Path)
	assert.Equal(t, "LEAF\nKEY\n", parsed.Members[0].PEM)
}

func TestCertFilesToSDK_SortedAndNilSafe(t *testing.T) {
	assert.Nil(t, certFilesToSDK(nil))

	out := certFilesToSDK(map[string]string{"/b.pem": "B", "/a.pem": "A"})
	require.Len(t, out, 2)
	assert.Equal(t, "/a.pem", *out[0].Path)
	assert.Equal(t, "A", *out[0].PEM)
	assert.Equal(t, "/b.pem", *out[1].Path)
}
