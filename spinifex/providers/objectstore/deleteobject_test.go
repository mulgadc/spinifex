package objectstore

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/s3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const noSuchKeyBody = `<?xml version="1.0" encoding="UTF-8"?>
<Error><Code>NoSuchKey</Code><Message>The specified key does not exist</Message></Error>`

// deleteResponder answers every DELETE with a fixed status and body, standing in
// for a backend that has already dropped the key.
type deleteResponder struct {
	code int
	body string
}

func (d *deleteResponder) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodDelete {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	w.WriteHeader(d.code)
	if d.body != "" {
		_, _ = w.Write([]byte(d.body))
	}
}

func TestS3ObjectStoreDeleteObjectMissingKey(t *testing.T) {
	tests := []struct {
		name string
		code int
		body string
	}{
		{name: "NoSuchKey body", code: http.StatusNotFound, body: noSuchKeyBody},
		{name: "bare 404", code: http.StatusNotFound},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(&deleteResponder{code: tt.code, body: tt.body})
			defer srv.Close()

			_, err := newS3Store(t, srv.URL).DeleteObject(context.Background(), &s3.DeleteObjectInput{
				Bucket: aws.String("bucket"),
				Key:    aws.String("vol-abc/chunks/chunk.00000028.bin"),
			})

			require.Error(t, err)
			assert.True(t, IsNoSuchKeyError(err), "want NoSuchKeyError, got %v", err)
		})
	}
}

func TestS3ObjectStoreDeleteObjectPropagatesOtherErrors(t *testing.T) {
	srv := httptest.NewServer(&deleteResponder{code: http.StatusInternalServerError})
	defer srv.Close()

	_, err := newS3Store(t, srv.URL).DeleteObject(context.Background(), &s3.DeleteObjectInput{
		Bucket: aws.String("bucket"),
		Key:    aws.String("vol-abc/chunks/chunk.00000028.bin"),
	})

	require.Error(t, err)
	assert.False(t, IsNoSuchKeyError(err), "a backend failure must not read as a missing key")
}

func TestS3ObjectStoreDeleteObjectSucceeds(t *testing.T) {
	srv := httptest.NewServer(&deleteResponder{code: http.StatusNoContent})
	defer srv.Close()

	_, err := newS3Store(t, srv.URL).DeleteObject(context.Background(), &s3.DeleteObjectInput{
		Bucket: aws.String("bucket"),
		Key:    aws.String("vol-abc/chunks/chunk.00000028.bin"),
	})

	require.NoError(t, err)
}
