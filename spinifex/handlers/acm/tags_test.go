package handlers_acm

import (
	"context"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/acm"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func importTagged(t *testing.T, svc *ACMServiceImpl, tags ...*acm.Tag) string {
	t.Helper()
	certPEM, keyPEM := genCert(t, "tags.example.com", "tags.example.com")
	out, err := svc.ImportCertificate(context.Background(), &acm.ImportCertificateInput{
		Certificate: certPEM,
		PrivateKey:  keyPEM,
		Tags:        tags,
	}, testAccountID)
	require.NoError(t, err)
	return aws.StringValue(out.CertificateArn)
}

func TestListTagsForCertificate_ReturnsImportTags(t *testing.T) {
	svc := setupACMService(t)
	arn := importTagged(t, svc, &acm.Tag{Key: aws.String("Name"), Value: aws.String("ingress")})

	out, err := svc.ListTagsForCertificate(context.Background(), &acm.ListTagsForCertificateInput{
		CertificateArn: aws.String(arn),
	}, testAccountID)
	require.NoError(t, err)
	require.Len(t, out.Tags, 1)
	assert.Equal(t, "Name", aws.StringValue(out.Tags[0].Key))
	assert.Equal(t, "ingress", aws.StringValue(out.Tags[0].Value))
}

func TestListTagsForCertificate_NoTagsReturnsEmpty(t *testing.T) {
	svc := setupACMService(t)
	arn := importTagged(t, svc)

	out, err := svc.ListTagsForCertificate(context.Background(), &acm.ListTagsForCertificateInput{
		CertificateArn: aws.String(arn),
	}, testAccountID)
	require.NoError(t, err)
	assert.Empty(t, out.Tags)
}

func TestAddTagsToCertificate_MergesAndPersists(t *testing.T) {
	svc := setupACMService(t)
	arn := importTagged(t, svc, &acm.Tag{Key: aws.String("Name"), Value: aws.String("ingress")})

	_, err := svc.AddTagsToCertificate(context.Background(), &acm.AddTagsToCertificateInput{
		CertificateArn: aws.String(arn),
		Tags: []*acm.Tag{
			{Key: aws.String("env"), Value: aws.String("dev")},
			{Key: aws.String("Name"), Value: aws.String("ingress-2")},
		},
	}, testAccountID)
	require.NoError(t, err)

	out, err := svc.ListTagsForCertificate(context.Background(), &acm.ListTagsForCertificateInput{
		CertificateArn: aws.String(arn),
	}, testAccountID)
	require.NoError(t, err)
	got := map[string]string{}
	for _, tg := range out.Tags {
		got[aws.StringValue(tg.Key)] = aws.StringValue(tg.Value)
	}
	assert.Equal(t, map[string]string{"Name": "ingress-2", "env": "dev"}, got)
}

func TestRemoveTagsFromCertificate_ByKeyAndByValue(t *testing.T) {
	svc := setupACMService(t)
	arn := importTagged(t, svc,
		&acm.Tag{Key: aws.String("Name"), Value: aws.String("ingress")},
		&acm.Tag{Key: aws.String("env"), Value: aws.String("dev")},
		&acm.Tag{Key: aws.String("team"), Value: aws.String("infra")},
	)

	// Key-only removal drops "Name"; value-mismatch keeps "env"; exact match drops "team".
	_, err := svc.RemoveTagsFromCertificate(context.Background(), &acm.RemoveTagsFromCertificateInput{
		CertificateArn: aws.String(arn),
		Tags: []*acm.Tag{
			{Key: aws.String("Name")},
			{Key: aws.String("env"), Value: aws.String("prod")},
			{Key: aws.String("team"), Value: aws.String("infra")},
		},
	}, testAccountID)
	require.NoError(t, err)

	out, err := svc.ListTagsForCertificate(context.Background(), &acm.ListTagsForCertificateInput{
		CertificateArn: aws.String(arn),
	}, testAccountID)
	require.NoError(t, err)
	require.Len(t, out.Tags, 1)
	assert.Equal(t, "env", aws.StringValue(out.Tags[0].Key))
	assert.Equal(t, "dev", aws.StringValue(out.Tags[0].Value))
}

func TestTagOps_CrossAccountHidden(t *testing.T) {
	svc := setupACMService(t)
	arn := importTagged(t, svc, &acm.Tag{Key: aws.String("Name"), Value: aws.String("ingress")})

	_, err := svc.ListTagsForCertificate(context.Background(), &acm.ListTagsForCertificateInput{
		CertificateArn: aws.String(arn),
	}, "000000000002")
	assert.Equal(t, awserrors.ErrorResourceNotFound, err.Error())
}
