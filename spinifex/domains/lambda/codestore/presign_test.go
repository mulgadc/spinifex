package codestore

import (
	"errors"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/credentials"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/s3"
	ststypes "github.com/aws/aws-sdk-go/service/sts"
	"github.com/mulgadc/spinifex/spinifex/providers/objectstore"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestStore_PresignGet(t *testing.T) {
	sess, err := session.NewSession(&aws.Config{
		Endpoint:         aws.String("https://predastore.test:8443"),
		Region:           aws.String("ap-southeast-2"),
		Credentials:      credentials.NewStaticCredentials("AKIATEST", "secret", "session-token"),
		S3ForcePathStyle: aws.Bool(true),
	})
	require.NoError(t, err)
	s := newStore(testRegion, objectstore.NewMemoryObjectStore())
	s.presign = presignWith(s3.New(sess))
	sum := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

	raw, err := s.PresignGet(t.Context(), testRef, sum, 10*time.Minute)
	require.NoError(t, err)

	u, err := url.Parse(raw)
	require.NoError(t, err)
	assert.Equal(t, "predastore.test:8443", u.Host)
	assert.Equal(t, "/"+BucketName(testRegion)+"/"+copyKey(sum), u.Path)
	q := u.Query()
	assert.Equal(t, "600", q.Get("X-Amz-Expires"))
	assert.Equal(t, "session-token", q.Get("X-Amz-Security-Token"))
	assert.NotEmpty(t, q.Get("X-Amz-Signature"))
}

func TestStore_PresignGetRefusals(t *testing.T) {
	s := newStore(testRegion, objectstore.NewMemoryObjectStore())
	sum := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

	_, err := s.PresignGet(t.Context(), testRef, sum, time.Minute)
	require.Error(t, err, "a store without a signer cannot presign")

	s.presign = func(*s3.GetObjectInput, time.Duration) (string, error) { return "unused", nil }
	_, err = s.PresignGet(t.Context(), testRef, "not-a-digest", time.Minute)
	require.ErrorIs(t, err, ErrInvalidRef)
}

func presignStoreOn(t *testing.T, c *clock, a *fakeAssume) *Store {
	t.Helper()
	provider := newSessionProvider(a.assume, c.now)
	creds := credentials.NewCredentials(provider)
	sess, err := session.NewSession(&aws.Config{
		Endpoint: aws.String("https://predastore.test:8443"), Region: aws.String("ap-southeast-2"),
		Credentials: creds, S3ForcePathStyle: aws.Bool(true),
	})
	require.NoError(t, err)
	s := newStore(testRegion, objectstore.NewMemoryObjectStore())
	s.presign = presignWith(s3.New(sess))
	s.creds, s.provider, s.now = creds, provider, c.now
	return s
}

func signedKeyID(t *testing.T, raw string) string {
	t.Helper()
	u, err := url.Parse(raw)
	require.NoError(t, err)
	return strings.SplitN(u.Query().Get("X-Amz-Credential"), "/", 2)[0]
}

func TestStore_PresignGetRefreshesAShortSession(t *testing.T) {
	sum := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	c := &clock{t: time.Unix(1_000_000, 0)}
	a := &fakeAssume{expires: c.t.Add(7 * time.Minute)}
	s := presignStoreOn(t, c, a)
	_, err := s.creds.Get()
	require.NoError(t, err)
	first := "ASIAB"
	require.Equal(t, 1, a.calls)

	a.expires = c.t.Add(time.Hour)
	raw, err := s.PresignGet(t.Context(), testRef, sum, 10*time.Minute)
	require.NoError(t, err)
	assert.Equal(t, 2, a.calls, "a 7 minute session is re-assumed before a 10 minute signature")
	assert.NotEqual(t, first, signedKeyID(t, raw))
	assert.Equal(t, "ASIAC", signedKeyID(t, raw), "the URL is signed with the new session")

	_, err = s.PresignGet(t.Context(), testRef, sum, 10*time.Minute)
	require.NoError(t, err)
	assert.Equal(t, 2, a.calls, "a long session is reused")
}

func TestStore_PresignGetRefusesWhenSessionStaysShort(t *testing.T) {
	sum := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	c := &clock{t: time.Unix(1_000_000, 0)}
	a := &fakeAssume{expires: c.t.Add(7 * time.Minute)}
	s := presignStoreOn(t, c, a)

	_, err := s.PresignGet(t.Context(), testRef, sum, 10*time.Minute)
	require.Error(t, err, "a re-assume that is still short signs nothing")
	assert.Equal(t, 2, a.calls)

	a.err = errors.New("AccessDenied")
	_, err = s.PresignGet(t.Context(), testRef, sum, 10*time.Minute)
	require.Error(t, err, "a failed re-assume does not fall back to the short session")
}

// Open signs download URLs for the off-host endpoint while its own reads and
// writes keep the local one, so a client outside the node can fetch the copy.
func TestOpen_PresignsForTheOffHostEndpoint(t *testing.T) {
	assume := func() (*ststypes.AssumeRoleOutput, error) {
		return &ststypes.AssumeRoleOutput{Credentials: &ststypes.Credentials{
			AccessKeyId:     aws.String("ASIATEST"),
			SecretAccessKey: aws.String("secret"),
			SessionToken:    aws.String("session-token"),
			Expiration:      aws.Time(time.Now().Add(time.Hour)),
		}}, nil
	}
	s, err := Open(OpenConfig{
		Region:          testRegion,
		Endpoint:        "https://127.0.0.1:8443",
		SigningRegion:   "ap-southeast-2",
		Assume:          assume,
		PresignEndpoint: "https://192.0.2.15:8443",
	})
	require.NoError(t, err)

	raw, err := s.PresignGet(t.Context(), testRef, strings.Repeat("a", 64), 10*time.Minute)
	require.NoError(t, err)
	u, err := url.Parse(raw)
	require.NoError(t, err)
	assert.Equal(t, "192.0.2.15:8443", u.Host)
}

func TestOpen_NoOffHostEndpointRefusesDownloadURLs(t *testing.T) {
	assume := func() (*ststypes.AssumeRoleOutput, error) {
		return &ststypes.AssumeRoleOutput{Credentials: &ststypes.Credentials{
			AccessKeyId:     aws.String("ASIATEST"),
			SecretAccessKey: aws.String("secret"),
			SessionToken:    aws.String("session-token"),
			Expiration:      aws.Time(time.Now().Add(time.Hour)),
		}}, nil
	}
	s, err := Open(OpenConfig{Region: testRegion, Endpoint: "https://127.0.0.1:8443", SigningRegion: "ap-southeast-2", Assume: assume})
	require.NoError(t, err)

	raw, err := s.PresignGet(t.Context(), testRef, strings.Repeat("a", 64), 10*time.Minute)
	require.ErrorIs(t, err, ErrNoPresignEndpoint)
	assert.Empty(t, raw)
}
