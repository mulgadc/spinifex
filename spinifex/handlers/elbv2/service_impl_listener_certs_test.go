package handlers_elbv2

import (
	"context"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/elbv2"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const testCertArn = "arn:aws:acm:ap-southeast-2:000000000001:certificate/aaaaaaaa-1111"
const testCertArn2 = "arn:aws:acm:ap-southeast-2:000000000001:certificate/bbbbbbbb-2222"

// fixedResponseAction is a valid default action that needs no target group.
func fixedResponseAction() []*elbv2.Action {
	return []*elbv2.Action{{
		Type: aws.String(ActionTypeFixedResponse),
		FixedResponseConfig: &elbv2.FixedResponseActionConfig{
			StatusCode: aws.String("200"),
		},
	}}
}

func createHTTPSListener(t *testing.T, svc *ELBv2ServiceImpl) string {
	t.Helper()
	lbArn := createLBArn(t, svc, "https-lb")
	out, err := svc.CreateListener(context.Background(), &elbv2.CreateListenerInput{
		LoadBalancerArn: aws.String(lbArn),
		Protocol:        aws.String(ProtocolHTTPS),
		Port:            aws.Int64(443),
		Certificates:    []*elbv2.Certificate{{CertificateArn: aws.String(testCertArn)}},
		DefaultActions:  fixedResponseAction(),
	}, testAccountID)
	require.NoError(t, err)
	return *out.Listeners[0].ListenerArn
}

func TestCreateListener_HTTPSRequiresCert(t *testing.T) {
	svc := setupTestService(t)
	lbArn := createLBArn(t, svc, "https-nocert")

	_, err := svc.CreateListener(context.Background(), &elbv2.CreateListenerInput{
		LoadBalancerArn: aws.String(lbArn),
		Protocol:        aws.String(ProtocolHTTPS),
		Port:            aws.Int64(443),
		DefaultActions:  fixedResponseAction(),
	}, testAccountID)
	require.Error(t, err)
	assert.Contains(t, err.Error(), awserrors.ErrorELBv2CertificateNotFound)
}

func TestCreateListener_HTTPSDefaultsSslPolicy(t *testing.T) {
	svc := setupTestService(t)
	lbArn := createLBArn(t, svc, "https-default-policy")

	out, err := svc.CreateListener(context.Background(), &elbv2.CreateListenerInput{
		LoadBalancerArn: aws.String(lbArn),
		Protocol:        aws.String(ProtocolHTTPS),
		Port:            aws.Int64(443),
		Certificates:    []*elbv2.Certificate{{CertificateArn: aws.String(testCertArn)}},
		DefaultActions:  fixedResponseAction(),
	}, testAccountID)
	require.NoError(t, err)

	l := out.Listeners[0]
	assert.Equal(t, DefaultSslPolicy, aws.StringValue(l.SslPolicy))
	require.Len(t, l.Certificates, 1)
	assert.Equal(t, testCertArn, aws.StringValue(l.Certificates[0].CertificateArn))
	assert.True(t, aws.BoolValue(l.Certificates[0].IsDefault), "the sole cert must be marked default")

	// DescribeListeners round-trips certificates and SslPolicy.
	desc, err := svc.DescribeListeners(context.Background(), &elbv2.DescribeListenersInput{
		ListenerArns: []*string{l.ListenerArn},
	}, testAccountID)
	require.NoError(t, err)
	require.Len(t, desc.Listeners, 1)
	assert.Equal(t, DefaultSslPolicy, aws.StringValue(desc.Listeners[0].SslPolicy))
	require.Len(t, desc.Listeners[0].Certificates, 1)
}

func TestCreateListener_HTTPRejectsCert(t *testing.T) {
	svc := setupTestService(t)
	lbArn := createLBArn(t, svc, "http-withcert")

	_, err := svc.CreateListener(context.Background(), &elbv2.CreateListenerInput{
		LoadBalancerArn: aws.String(lbArn),
		Protocol:        aws.String(ProtocolHTTP),
		Port:            aws.Int64(80),
		Certificates:    []*elbv2.Certificate{{CertificateArn: aws.String(testCertArn)}},
		DefaultActions:  fixedResponseAction(),
	}, testAccountID)
	require.Error(t, err)
	assert.Contains(t, err.Error(), awserrors.ErrorInvalidParameterValue)
}

func TestCreateListener_UnknownSslPolicy(t *testing.T) {
	svc := setupTestService(t)
	lbArn := createLBArn(t, svc, "https-badpolicy")

	_, err := svc.CreateListener(context.Background(), &elbv2.CreateListenerInput{
		LoadBalancerArn: aws.String(lbArn),
		Protocol:        aws.String(ProtocolHTTPS),
		Port:            aws.Int64(443),
		Certificates:    []*elbv2.Certificate{{CertificateArn: aws.String(testCertArn)}},
		SslPolicy:       aws.String("ELBSecurityPolicy-Does-Not-Exist"),
		DefaultActions:  fixedResponseAction(),
	}, testAccountID)
	require.Error(t, err)
	assert.Contains(t, err.Error(), awserrors.ErrorELBv2SSLPolicyNotFound)
}

func TestModifyListener_SwitchToHTTPSRequiresCert(t *testing.T) {
	svc := setupTestService(t)
	lbArn := createLBArn(t, svc, "switch-lb")
	lst, err := svc.CreateListener(context.Background(), &elbv2.CreateListenerInput{
		LoadBalancerArn: aws.String(lbArn),
		Protocol:        aws.String(ProtocolHTTP),
		Port:            aws.Int64(80),
		DefaultActions:  fixedResponseAction(),
	}, testAccountID)
	require.NoError(t, err)
	arn := lst.Listeners[0].ListenerArn

	// HTTP -> HTTPS without a cert is rejected.
	_, err = svc.ModifyListener(context.Background(), &elbv2.ModifyListenerInput{
		ListenerArn: arn,
		Protocol:    aws.String(ProtocolHTTPS),
	}, testAccountID)
	require.Error(t, err)
	assert.Contains(t, err.Error(), awserrors.ErrorELBv2CertificateNotFound)

	// HTTP -> HTTPS with a cert succeeds and defaults the policy.
	out, err := svc.ModifyListener(context.Background(), &elbv2.ModifyListenerInput{
		ListenerArn:  arn,
		Protocol:     aws.String(ProtocolHTTPS),
		Certificates: []*elbv2.Certificate{{CertificateArn: aws.String(testCertArn)}},
	}, testAccountID)
	require.NoError(t, err)
	assert.Equal(t, DefaultSslPolicy, aws.StringValue(out.Listeners[0].SslPolicy))
	require.Len(t, out.Listeners[0].Certificates, 1)
}

func TestModifyListener_SwitchAwayClearsCerts(t *testing.T) {
	svc := setupTestService(t)
	arn := createHTTPSListener(t, svc)

	out, err := svc.ModifyListener(context.Background(), &elbv2.ModifyListenerInput{
		ListenerArn: aws.String(arn),
		Protocol:    aws.String(ProtocolHTTP),
	}, testAccountID)
	require.NoError(t, err)
	assert.Empty(t, out.Listeners[0].Certificates, "certs cleared when leaving a secure protocol")
	assert.Nil(t, out.Listeners[0].SslPolicy)
}

func TestListenerCertificates_AddRemoveDescribe(t *testing.T) {
	svc := setupTestService(t)
	arn := createHTTPSListener(t, svc)

	// Add an SNI cert.
	addOut, err := svc.AddListenerCertificates(context.Background(), &elbv2.AddListenerCertificatesInput{
		ListenerArn:  aws.String(arn),
		Certificates: []*elbv2.Certificate{{CertificateArn: aws.String(testCertArn2)}},
	}, testAccountID)
	require.NoError(t, err)
	require.Len(t, addOut.Certificates, 2)

	// Re-adding is idempotent.
	addOut, err = svc.AddListenerCertificates(context.Background(), &elbv2.AddListenerCertificatesInput{
		ListenerArn:  aws.String(arn),
		Certificates: []*elbv2.Certificate{{CertificateArn: aws.String(testCertArn2)}},
	}, testAccountID)
	require.NoError(t, err)
	require.Len(t, addOut.Certificates, 2)

	// Describe shows both.
	desc, err := svc.DescribeListenerCertificates(context.Background(), &elbv2.DescribeListenerCertificatesInput{
		ListenerArn: aws.String(arn),
	}, testAccountID)
	require.NoError(t, err)
	require.Len(t, desc.Certificates, 2)

	// Removing the default cert is rejected.
	_, err = svc.RemoveListenerCertificates(context.Background(), &elbv2.RemoveListenerCertificatesInput{
		ListenerArn:  aws.String(arn),
		Certificates: []*elbv2.Certificate{{CertificateArn: aws.String(testCertArn)}},
	}, testAccountID)
	require.Error(t, err)
	assert.Contains(t, err.Error(), awserrors.ErrorELBv2InvalidConfigurationRequest)

	// Removing the SNI cert succeeds.
	_, err = svc.RemoveListenerCertificates(context.Background(), &elbv2.RemoveListenerCertificatesInput{
		ListenerArn:  aws.String(arn),
		Certificates: []*elbv2.Certificate{{CertificateArn: aws.String(testCertArn2)}},
	}, testAccountID)
	require.NoError(t, err)

	desc, err = svc.DescribeListenerCertificates(context.Background(), &elbv2.DescribeListenerCertificatesInput{
		ListenerArn: aws.String(arn),
	}, testAccountID)
	require.NoError(t, err)
	require.Len(t, desc.Certificates, 1)
	assert.Equal(t, testCertArn, aws.StringValue(desc.Certificates[0].CertificateArn))
}

func TestAddListenerCertificates_HTTPRejected(t *testing.T) {
	svc := setupTestService(t)
	lbArn := createLBArn(t, svc, "http-addcert")
	lst, err := svc.CreateListener(context.Background(), &elbv2.CreateListenerInput{
		LoadBalancerArn: aws.String(lbArn),
		Protocol:        aws.String(ProtocolHTTP),
		Port:            aws.Int64(80),
		DefaultActions:  fixedResponseAction(),
	}, testAccountID)
	require.NoError(t, err)

	_, err = svc.AddListenerCertificates(context.Background(), &elbv2.AddListenerCertificatesInput{
		ListenerArn:  lst.Listeners[0].ListenerArn,
		Certificates: []*elbv2.Certificate{{CertificateArn: aws.String(testCertArn)}},
	}, testAccountID)
	require.Error(t, err)
	assert.Contains(t, err.Error(), awserrors.ErrorELBv2InvalidConfigurationRequest)
}

func TestListenerCertificates_ValidationAndNotFound(t *testing.T) {
	svc := setupTestService(t)
	arn := createHTTPSListener(t, svc)
	const otherAccount = "999999999999"
	badArn := "arn:aws:elasticloadbalancing:us-east-1:000000000001:listener/app/x/y/z"

	// Add: missing arn, empty certs, nil cert entry, cross-account not-found.
	_, err := svc.AddListenerCertificates(context.Background(), &elbv2.AddListenerCertificatesInput{}, testAccountID)
	assert.EqualError(t, err, awserrors.ErrorMissingParameter)

	_, err = svc.AddListenerCertificates(context.Background(), &elbv2.AddListenerCertificatesInput{
		ListenerArn: aws.String(arn),
	}, testAccountID)
	assert.EqualError(t, err, awserrors.ErrorMissingParameter)

	_, err = svc.AddListenerCertificates(context.Background(), &elbv2.AddListenerCertificatesInput{
		ListenerArn:  aws.String(arn),
		Certificates: []*elbv2.Certificate{{}},
	}, testAccountID)
	assert.EqualError(t, err, awserrors.ErrorInvalidParameterValue)

	_, err = svc.AddListenerCertificates(context.Background(), &elbv2.AddListenerCertificatesInput{
		ListenerArn:  aws.String(arn),
		Certificates: []*elbv2.Certificate{{CertificateArn: aws.String(testCertArn2)}},
	}, otherAccount)
	assert.EqualError(t, err, awserrors.ErrorELBv2ListenerNotFound)

	// Remove: missing arn, empty certs, nil cert entry, cross-account not-found.
	_, err = svc.RemoveListenerCertificates(context.Background(), &elbv2.RemoveListenerCertificatesInput{}, testAccountID)
	assert.EqualError(t, err, awserrors.ErrorMissingParameter)

	_, err = svc.RemoveListenerCertificates(context.Background(), &elbv2.RemoveListenerCertificatesInput{
		ListenerArn: aws.String(arn),
	}, testAccountID)
	assert.EqualError(t, err, awserrors.ErrorMissingParameter)

	_, err = svc.RemoveListenerCertificates(context.Background(), &elbv2.RemoveListenerCertificatesInput{
		ListenerArn:  aws.String(arn),
		Certificates: []*elbv2.Certificate{{}},
	}, testAccountID)
	assert.EqualError(t, err, awserrors.ErrorInvalidParameterValue)

	_, err = svc.RemoveListenerCertificates(context.Background(), &elbv2.RemoveListenerCertificatesInput{
		ListenerArn:  aws.String(badArn),
		Certificates: []*elbv2.Certificate{{CertificateArn: aws.String(testCertArn)}},
	}, testAccountID)
	assert.EqualError(t, err, awserrors.ErrorELBv2ListenerNotFound)

	// Describe: missing arn, cross-account not-found.
	_, err = svc.DescribeListenerCertificates(context.Background(), &elbv2.DescribeListenerCertificatesInput{}, testAccountID)
	assert.EqualError(t, err, awserrors.ErrorMissingParameter)

	_, err = svc.DescribeListenerCertificates(context.Background(), &elbv2.DescribeListenerCertificatesInput{
		ListenerArn: aws.String(arn),
	}, otherAccount)
	assert.EqualError(t, err, awserrors.ErrorELBv2ListenerNotFound)
}

func TestDescribeSSLPolicies_EmptyName(t *testing.T) {
	svc := setupTestService(t)
	_, err := svc.DescribeSSLPolicies(context.Background(), &elbv2.DescribeSSLPoliciesInput{
		Names: []*string{aws.String("")},
	}, testAccountID)
	assert.EqualError(t, err, awserrors.ErrorInvalidParameterValue)
}

func TestDescribeSSLPolicies(t *testing.T) {
	svc := setupTestService(t)

	// No filter → full catalog.
	all, err := svc.DescribeSSLPolicies(context.Background(), &elbv2.DescribeSSLPoliciesInput{}, testAccountID)
	require.NoError(t, err)
	require.Len(t, all.SslPolicies, len(sslPolicyOrder))
	assert.Equal(t, DefaultSslPolicy, aws.StringValue(all.SslPolicies[0].Name))
	require.NotEmpty(t, all.SslPolicies[0].Ciphers)
	require.NotEmpty(t, all.SslPolicies[0].SslProtocols)

	// Name filter → subset.
	one, err := svc.DescribeSSLPolicies(context.Background(), &elbv2.DescribeSSLPoliciesInput{
		Names: []*string{aws.String(DefaultSslPolicy)},
	}, testAccountID)
	require.NoError(t, err)
	require.Len(t, one.SslPolicies, 1)

	// Unknown name → error.
	_, err = svc.DescribeSSLPolicies(context.Background(), &elbv2.DescribeSSLPoliciesInput{
		Names: []*string{aws.String("nope")},
	}, testAccountID)
	require.Error(t, err)
	assert.Contains(t, err.Error(), awserrors.ErrorELBv2SSLPolicyNotFound)
}
