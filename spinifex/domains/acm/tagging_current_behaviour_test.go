package acm

import (
	"context"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/acm"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The TestCurrentBehaviour_Tagging_ tests pin what the ACM tag writers do to
// the certificate record today. They describe current behaviour, not the
// target design, and a failure means the behaviour changed.

// The tag writers change only the stored tag field, so the private key and
// certificate body survive a tag or untag unchanged.
func TestCurrentBehaviour_Tagging_ACMTagWritesPreserveStoredPrivateKey(t *testing.T) {
	cases := map[string]func(svc *ACMServiceImpl, certArn string) error{
		"AddTagsToCertificate": func(svc *ACMServiceImpl, certArn string) error {
			_, err := svc.AddTagsToCertificate(context.Background(), &acm.AddTagsToCertificateInput{
				CertificateArn: aws.String(certArn),
				Tags:           []*acm.Tag{{Key: aws.String("env"), Value: aws.String("dev")}},
			}, testAccountID)
			return err
		},
		"RemoveTagsFromCertificate": func(svc *ACMServiceImpl, certArn string) error {
			_, err := svc.RemoveTagsFromCertificate(context.Background(), &acm.RemoveTagsFromCertificateInput{
				CertificateArn: aws.String(certArn),
				Tags:           []*acm.Tag{{Key: aws.String("Name")}},
			}, testAccountID)
			return err
		},
	}
	for name, write := range cases {
		t.Run(name, func(t *testing.T) {
			svc := setupACMService(t)
			certArn := importTagged(t, svc, &acm.Tag{Key: aws.String("Name"), Value: aws.String("ingress")})

			before, err := svc.store.GetCert(context.Background(), certArn)
			require.NoError(t, err)
			require.NotEmpty(t, before.PrivateKey, "import must store a usable key")

			require.NoError(t, write(svc, certArn))

			after, err := svc.store.GetCert(context.Background(), certArn)
			require.NoError(t, err)
			assert.Equal(t, before.PrivateKey, after.PrivateKey, "a tag write keeps the stored private key")
			assert.Equal(t, before.Certificate, after.Certificate, "the certificate body survives")
		})
	}
}

// Neither tag writer bounds the number of tags, the key or value length, or
// refuses the reserved aws: prefix; every tag supplied is stored.
func TestCurrentBehaviour_Tagging_ACMAddTagsAppliesNoTagLimits(t *testing.T) {
	svc := setupACMService(t)
	certArn := importTagged(t, svc)

	tags := []*acm.Tag{
		{Key: aws.String("aws:reserved"), Value: aws.String("v")},
		{Key: aws.String(longString(129)), Value: aws.String(longString(257))},
	}
	for i := range 50 {
		tags = append(tags, &acm.Tag{Key: aws.String("k" + longString(i+1)), Value: aws.String("v")})
	}
	_, err := svc.AddTagsToCertificate(context.Background(), &acm.AddTagsToCertificateInput{
		CertificateArn: aws.String(certArn),
		Tags:           tags,
	}, testAccountID)
	require.NoError(t, err)

	got := listTagMap(t, svc, certArn)
	assert.Len(t, got, 52)
	assert.Contains(t, got, "aws:reserved")
	assert.Equal(t, longString(257), got[longString(129)])
}

func longString(n int) string {
	b := make([]byte, n)
	for i := range b {
		b[i] = 'a'
	}
	return string(b)
}
