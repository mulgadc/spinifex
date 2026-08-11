package handlers_ec2_volume

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
)

// fakeS3Host is the endpoint the test volume services point Predastore at.
// Pointing it at an unserved port makes every CreateVolume burn the AWS SDK
// retry backoff in Backend.Init before failing.
var fakeS3Host string

// TestMain stands up one in-process S3 stand-in for the package: ListObjectsV2
// succeeds so Backend.Init's reachability probe returns immediately, and every
// other request 404s so CreateVolume still fails at the storage layer.
func TestMain(m *testing.M) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/xml")
		if r.URL.Query().Get("list-type") == "2" {
			w.WriteHeader(http.StatusOK)
			io.WriteString(w, `<?xml version="1.0" encoding="UTF-8"?>`+
				`<ListBucketResult xmlns="http://s3.amazonaws.com/doc/2006-03-01/">`+
				`<Name>test-bucket</Name><KeyCount>0</KeyCount>`+
				`<MaxKeys>1000</MaxKeys><IsTruncated>false</IsTruncated></ListBucketResult>`)
			return
		}
		w.WriteHeader(http.StatusNotFound)
		io.WriteString(w, `<?xml version="1.0" encoding="UTF-8"?>`+
			`<Error><Code>NoSuchKey</Code>`+
			`<Message>The specified key does not exist.</Message></Error>`)
	}))
	fakeS3Host = srv.URL

	code := m.Run()

	srv.Close()
	os.Exit(code)
}
