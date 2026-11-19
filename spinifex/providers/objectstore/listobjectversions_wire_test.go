package objectstore

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/s3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// testdata/list_object_versions.xml is predastore's own pinned response body,
// byte for byte. Its source is predastore/testdata/list_object_versions.xml,
// which a whole-stack test over real HTTP writes and compares, so a drift in
// predastore's XML shows up as a diff in both repositories rather than as a
// teardown that silently reads no versions.
const versionsFixture = "testdata/list_object_versions.xml"

// The pinned body normalises every version id to one placeholder so the
// comparison does not depend on generated ids. Substituting two distinct ones
// here is what lets the assertions tell the version from the marker.
const (
	fixtureVersionID = "v-9HfTqvZ"
	fixtureMarkerID  = "m-2KdLpWr"
)

// versionsResponder answers the version listing with the pinned body.
type versionsResponder struct {
	body     string
	gotQuery string
}

func (v *versionsResponder) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	v.gotQuery = r.URL.RawQuery
	w.Header().Set("Content-Type", "application/xml")
	_, _ = w.Write([]byte(v.body))
}

func fixtureBody(t *testing.T) string {
	t.Helper()
	raw, err := os.ReadFile(versionsFixture)
	require.NoError(t, err)

	body := strings.Replace(string(raw), "VERSIONID", fixtureVersionID, 1)
	body = strings.Replace(body, "VERSIONID", fixtureMarkerID, 1)
	body = strings.ReplaceAll(body, "TIMESTAMP", "2026-11-19T04:21:08.000Z")
	require.NotContains(t, body, "VERSIONID", "the fixture no longer carries two version ids")
	return body
}

// The reaper reads versions and markers through aws-sdk-go v1, and a key the
// SDK does not recognise decodes to an empty slice rather than an error. That
// would read as an empty bucket and leave every version in place, so the
// decode is pinned against the body predastore actually sends.
func TestListObjectVersionsDecodesPredastoresBody(t *testing.T) {
	responder := &versionsResponder{body: fixtureBody(t)}
	srv := httptest.NewServer(responder)
	defer srv.Close()

	out, err := newS3Store(t, srv.URL).ListObjectVersions(context.Background(), &s3.ListObjectVersionsInput{
		Bucket: aws.String("emptied-by-version"),
	})
	require.NoError(t, err)

	require.Len(t, out.Versions, 1, "the retained version was not decoded")
	assert.Equal(t, "a.txt", aws.StringValue(out.Versions[0].Key))
	assert.Equal(t, fixtureVersionID, aws.StringValue(out.Versions[0].VersionId))
	assert.False(t, aws.BoolValue(out.Versions[0].IsLatest), "the hidden version is not the latest")
	assert.Equal(t, int64(13), aws.Int64Value(out.Versions[0].Size))
	// The pinned body escapes the quotes, so a decoder that passed the entity
	// through would hand the reaper an etag no comparison could match.
	assert.Equal(t, `"8f7d68ec8392d366f755ce30097c7bf5"`, aws.StringValue(out.Versions[0].ETag))

	require.Len(t, out.DeleteMarkers, 1, "the delete marker was not decoded")
	assert.Equal(t, "a.txt", aws.StringValue(out.DeleteMarkers[0].Key))
	assert.Equal(t, fixtureMarkerID, aws.StringValue(out.DeleteMarkers[0].VersionId))
	assert.True(t, aws.BoolValue(out.DeleteMarkers[0].IsLatest), "the marker is what hides the key")

	assert.False(t, aws.BoolValue(out.IsTruncated))
	assert.Equal(t, "emptied-by-version", aws.StringValue(out.Name))
	assert.Equal(t, int64(1000), aws.Int64Value(out.MaxKeys))
}

// The paging markers and the owner precondition have to reach the wire. A
// query parameter the SDK drops would restart the listing at the first page on
// every pass, and a missing header would empty a bucket with nothing asserted.
func TestListObjectVersionsSendsMarkersAndTheExpectedOwner(t *testing.T) {
	responder := &versionsResponder{body: fixtureBody(t)}
	srv := httptest.NewServer(responder)
	defer srv.Close()

	_, err := newS3Store(t, srv.URL).ListObjectVersions(context.Background(), &s3.ListObjectVersionsInput{
		Bucket:              aws.String("emptied-by-version"),
		KeyMarker:           aws.String("a.txt"),
		VersionIdMarker:     aws.String(fixtureVersionID),
		ExpectedBucketOwner: aws.String("000000000042"),
	})
	require.NoError(t, err)

	assert.Contains(t, responder.gotQuery, "versions")
	assert.Contains(t, responder.gotQuery, "key-marker=a.txt")
	assert.Contains(t, responder.gotQuery, "version-id-marker="+fixtureVersionID)
}

// Every call the bucket reaper makes turns its account into the expected owner
// header. The header name and the fact that it is signed are predastore's
// contract, so a rename on either side has to break here.
func TestExpectedOwnerReachesTheWireOnEveryBucketCall(t *testing.T) {
	var seen []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, r.Header.Get("X-Amz-Expected-Bucket-Owner"))
		signed := r.Header.Get("Authorization")
		assert.Contains(t, signed, "x-amz-expected-bucket-owner",
			"the precondition must be covered by the signature, not sent unsigned")
		w.Header().Set("Content-Type", "application/xml")
		_, _ = w.Write([]byte(`<ListVersionsResult></ListVersionsResult>`))
	}))
	defer srv.Close()

	store := newS3Store(t, srv.URL)
	const account = "000000000042"

	_, err := store.ListObjectVersions(context.Background(), &s3.ListObjectVersionsInput{
		Bucket:              aws.String("tenant-data"),
		ExpectedBucketOwner: aws.String(account),
	})
	require.NoError(t, err)
	_, err = store.ListMultipartUploads(context.Background(), "tenant-data", account)
	require.NoError(t, err)
	require.NoError(t, store.AbortMultipartUpload(context.Background(),
		MultipartUploadRef{Bucket: "tenant-data", Key: "big.img", UploadID: "u-1"}, account))
	_, err = store.GetBucketVersioning(context.Background(), "tenant-data", account)
	require.NoError(t, err)
	require.NoError(t, store.DeleteBucket(context.Background(), "tenant-data", account))

	require.Len(t, seen, 5)
	for i, got := range seen {
		assert.Equal(t, account, got, "request %d asserted no owner", i)
	}
}

// An unscoped caller asserts nothing, and a header carrying the empty string
// would be refused by every bucket rather than skipped.
func TestExpectedOwnerIsOmittedForAnEmptyAccount(t *testing.T) {
	var present bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, present = r.Header["X-Amz-Expected-Bucket-Owner"]
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	require.NoError(t, newS3Store(t, srv.URL).DeleteBucket(context.Background(), "tenant-data", ""))
	assert.False(t, present, "an empty account must send no precondition at all")
}
