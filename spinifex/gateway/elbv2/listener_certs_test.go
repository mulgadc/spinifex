package gateway_elbv2

import (
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/elbv2"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	"github.com/mulgadc/spinifex/spinifex/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const testListenerArn = "arn:aws:elasticloadbalancing:us-east-1:123456789012:listener/app/lb/abc/def"
const testCertArn = "arn:aws:acm:us-east-1:123456789012:certificate/aaaa-1111"

// Validation guards (no NATS call reached).

func TestAddListenerCertificates_NilInput(t *testing.T) {
	_, err := AddListenerCertificates(nil, nil, "123456789012")
	assert.EqualError(t, err, awserrors.ErrorMissingParameter)
}

func TestAddListenerCertificates_MissingArn(t *testing.T) {
	_, err := AddListenerCertificates(&elbv2.AddListenerCertificatesInput{
		Certificates: []*elbv2.Certificate{{CertificateArn: aws.String(testCertArn)}},
	}, nil, "123456789012")
	assert.EqualError(t, err, awserrors.ErrorMissingParameter)
}

func TestAddListenerCertificates_MissingCerts(t *testing.T) {
	_, err := AddListenerCertificates(&elbv2.AddListenerCertificatesInput{
		ListenerArn: aws.String(testListenerArn),
	}, nil, "123456789012")
	assert.EqualError(t, err, awserrors.ErrorMissingParameter)
}

func TestRemoveListenerCertificates_NilInput(t *testing.T) {
	_, err := RemoveListenerCertificates(nil, nil, "123456789012")
	assert.EqualError(t, err, awserrors.ErrorMissingParameter)
}

func TestRemoveListenerCertificates_MissingArn(t *testing.T) {
	_, err := RemoveListenerCertificates(&elbv2.RemoveListenerCertificatesInput{
		Certificates: []*elbv2.Certificate{{CertificateArn: aws.String(testCertArn)}},
	}, nil, "123456789012")
	assert.EqualError(t, err, awserrors.ErrorMissingParameter)
}

func TestRemoveListenerCertificates_MissingCerts(t *testing.T) {
	_, err := RemoveListenerCertificates(&elbv2.RemoveListenerCertificatesInput{
		ListenerArn: aws.String(testListenerArn),
	}, nil, "123456789012")
	assert.EqualError(t, err, awserrors.ErrorMissingParameter)
}

func TestDescribeListenerCertificates_NilInput(t *testing.T) {
	_, err := DescribeListenerCertificates(nil, nil, "123456789012")
	assert.EqualError(t, err, awserrors.ErrorMissingParameter)
}

func TestDescribeListenerCertificates_MissingArn(t *testing.T) {
	_, err := DescribeListenerCertificates(&elbv2.DescribeListenerCertificatesInput{}, nil, "123456789012")
	assert.EqualError(t, err, awserrors.ErrorMissingParameter)
}

// Delegation paths: valid input, no daemon — NATS fast-fails, exercising the error return path.

func TestAddListenerCertificates_DelegatesNoResponder(t *testing.T) {
	_, nc, _ := testutil.StartTestJetStream(t)
	_, err := AddListenerCertificates(&elbv2.AddListenerCertificatesInput{
		ListenerArn:  aws.String(testListenerArn),
		Certificates: []*elbv2.Certificate{{CertificateArn: aws.String(testCertArn)}},
	}, nc, "123456789012")
	require.Error(t, err)
}

func TestRemoveListenerCertificates_DelegatesNoResponder(t *testing.T) {
	_, nc, _ := testutil.StartTestJetStream(t)
	_, err := RemoveListenerCertificates(&elbv2.RemoveListenerCertificatesInput{
		ListenerArn:  aws.String(testListenerArn),
		Certificates: []*elbv2.Certificate{{CertificateArn: aws.String(testCertArn)}},
	}, nc, "123456789012")
	require.Error(t, err)
}

func TestDescribeListenerCertificates_DelegatesNoResponder(t *testing.T) {
	_, nc, _ := testutil.StartTestJetStream(t)
	_, err := DescribeListenerCertificates(&elbv2.DescribeListenerCertificatesInput{
		ListenerArn: aws.String(testListenerArn),
	}, nc, "123456789012")
	require.Error(t, err)
}

func TestDescribeSSLPolicies_DelegatesNoResponder(t *testing.T) {
	_, nc, _ := testutil.StartTestJetStream(t)
	_, err := DescribeSSLPolicies(&elbv2.DescribeSSLPoliciesInput{}, nc, "123456789012")
	require.Error(t, err)
}
