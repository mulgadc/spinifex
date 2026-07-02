package objectstore

import (
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/credentials"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/s3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeS3 records the HEAD/PUT calls EnsureBucket makes and replies with
// canned status/body so the create-when-absent logic can be exercised without
// a real predastore.
type fakeS3 struct {
	mu       sync.Mutex
	heads    int
	puts     int
	headCode int
	putCode  int
	putBody  string
}

func (f *fakeS3) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	switch r.Method {
	case http.MethodHead:
		f.heads++
		w.WriteHeader(f.headCode)
	case http.MethodPut:
		f.puts++
		if f.putBody != "" {
			w.WriteHeader(f.putCode)
			_, _ = w.Write([]byte(f.putBody))
			return
		}
		w.WriteHeader(f.putCode)
	default:
		w.WriteHeader(http.StatusBadRequest)
	}
}

func newS3Store(t *testing.T, url string) *S3ObjectStore {
	t.Helper()
	sess := session.Must(session.NewSession(&aws.Config{
		Endpoint:         aws.String(url),
		Region:           aws.String("ap-southeast-2"),
		Credentials:      credentials.NewStaticCredentials("ak", "sk", ""),
		S3ForcePathStyle: aws.Bool(true),
		DisableSSL:       aws.Bool(true),
	}))
	return NewS3ObjectStore(s3.New(sess))
}

func TestS3ObjectStore_EnsureBucket_HeadOK(t *testing.T) {
	fake := &fakeS3{headCode: http.StatusOK}
	srv := httptest.NewServer(fake)
	defer srv.Close()

	require.NoError(t, newS3Store(t, srv.URL).EnsureBucket("ecr-000000000001"))
	assert.Equal(t, 1, fake.heads)
	assert.Equal(t, 0, fake.puts, "present bucket must not be re-created")
}

func TestS3ObjectStore_EnsureBucket_CreatesWhenAbsent(t *testing.T) {
	fake := &fakeS3{headCode: http.StatusNotFound, putCode: http.StatusOK}
	srv := httptest.NewServer(fake)
	defer srv.Close()

	require.NoError(t, newS3Store(t, srv.URL).EnsureBucket("ecr-000000000001"))
	assert.Equal(t, 1, fake.heads)
	assert.Equal(t, 1, fake.puts)
}

func TestS3ObjectStore_EnsureBucket_AlreadyOwnedIsSuccess(t *testing.T) {
	fake := &fakeS3{
		headCode: http.StatusNotFound,
		putCode:  http.StatusConflict,
		putBody:  `<Error><Code>BucketAlreadyOwnedByYou</Code><Message>owned</Message></Error>`,
	}
	srv := httptest.NewServer(fake)
	defer srv.Close()

	require.NoError(t, newS3Store(t, srv.URL).EnsureBucket("ecr-000000000001"))
	assert.Equal(t, 1, fake.puts)
}

func TestS3ObjectStore_EnsureBucket_BackendErrorPropagates(t *testing.T) {
	fake := &fakeS3{
		headCode: http.StatusNotFound,
		putCode:  http.StatusInternalServerError,
		putBody:  `<Error><Code>InternalError</Code><Message>boom</Message></Error>`,
	}
	srv := httptest.NewServer(fake)
	defer srv.Close()

	require.Error(t, newS3Store(t, srv.URL).EnsureBucket("ecr-000000000001"))
}
