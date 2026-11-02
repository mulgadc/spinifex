package compress

import (
	"compress/gzip"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// largeXML is above the size threshold and is the shape this surface returns:
// repeated records, which is why compression pays here.
func largeXML() string {
	var b strings.Builder
	b.WriteString("<DescribeInstanceTypesResponse>")
	for range 200 {
		b.WriteString("<item><instanceType>t3.micro</instanceType></item>")
	}
	b.WriteString("</DescribeInstanceTypesResponse>")
	return b.String()
}

// serve runs one request through the middleware and returns the recorder.
func serve(t *testing.T, acceptEncoding string, h http.HandlerFunc) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	if acceptEncoding != "" {
		req.Header.Set("Accept-Encoding", acceptEncoding)
	}
	rec := httptest.NewRecorder()
	Middleware(h).ServeHTTP(rec, req)
	return rec
}

// gunzip fails the test rather than returning an error, so a body that is not
// valid gzip is reported where it happened.
func gunzip(t *testing.T, body io.Reader) string {
	t.Helper()
	zr, err := gzip.NewReader(body)
	require.NoError(t, err)
	out, err := io.ReadAll(zr)
	require.NoError(t, err)
	return string(out)
}

func TestLargeXMLIsCompressedAndRoundTrips(t *testing.T) {
	t.Parallel()
	want := largeXML()

	rec := serve(t, "gzip, deflate, br", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/xml")
		_, _ = w.Write([]byte(want))
	})

	assert.Equal(t, "gzip", rec.Header().Get("Content-Encoding"))
	assert.Contains(t, rec.Header().Values("Vary"), "Accept-Encoding")
	assert.Equal(t, want, gunzip(t, rec.Body))
	assert.Less(t, rec.Body.Len(), len(want)/4, "repeated XML must compress well")
}

// The saving is the reason for the middleware, so it is asserted rather than
// assumed.
func TestCompressedBodyIsSmallerThanTheOriginal(t *testing.T) {
	t.Parallel()
	want := largeXML()

	plain := serve(t, "", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/xml")
		_, _ = w.Write([]byte(want))
	})
	gzipped := serve(t, "gzip", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/xml")
		_, _ = w.Write([]byte(want))
	})

	assert.Empty(t, plain.Header().Get("Content-Encoding"))
	assert.Equal(t, len(want), plain.Body.Len())
	assert.Less(t, gzipped.Body.Len(), plain.Body.Len())
}

func TestClientThatDoesNotOfferGzipGetsPlainBytes(t *testing.T) {
	t.Parallel()
	want := largeXML()

	for name, accept := range map[string]string{
		"absent":   "",
		"deflate":  "deflate",
		"refusedQ": "gzip;q=0",
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			rec := serve(t, accept, func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "text/xml")
				_, _ = w.Write([]byte(want))
			})

			assert.Empty(t, rec.Header().Get("Content-Encoding"))
			assert.Equal(t, want, rec.Body.String())
		})
	}
}

// A short error envelope is the commonest reply on this surface, and gzipping
// it would add bytes.
func TestShortBodyIsNotCompressed(t *testing.T) {
	t.Parallel()
	const want = `<Response><Errors><Error><Code>InvalidAction</Code></Error></Errors></Response>`

	rec := serve(t, "gzip", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/xml")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(want))
	})

	assert.Empty(t, rec.Header().Get("Content-Encoding"))
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Equal(t, want, rec.Body.String())
}

// A handler that declared its own length has told the client how many bytes to
// expect, and the OCI Distribution clients enforce it.
func TestHandlerDeclaredContentLengthIsNotCompressed(t *testing.T) {
	t.Parallel()
	want := largeXML()

	rec := serve(t, "gzip", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Content-Length", strconv.Itoa(len(want)))
		_, _ = w.Write([]byte(want))
	})

	assert.Empty(t, rec.Header().Get("Content-Encoding"))
	assert.Equal(t, strconv.Itoa(len(want)), rec.Header().Get("Content-Length"))
	assert.Equal(t, want, rec.Body.String())
}

func TestAlreadyEncodedBodyIsNotReEncoded(t *testing.T) {
	t.Parallel()
	want := largeXML()

	rec := serve(t, "gzip", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Content-Encoding", "br")
		_, _ = w.Write([]byte(want))
	})

	assert.Equal(t, "br", rec.Header().Get("Content-Encoding"))
	assert.Equal(t, want, rec.Body.String())
}

// Each of these reaches a client byte for byte: the first two stream, and the
// rest are already compressed.
func TestPassThroughContentTypesAreNotCompressed(t *testing.T) {
	t.Parallel()
	want := largeXML()

	for _, contentType := range []string{
		"text/event-stream",
		"application/vnd.amazon.eventstream",
		"application/vnd.oci.image.layer.v1.tar+gzip",
		"application/vnd.docker.image.rootfs.diff.tar.gzip",
		"application/gzip",
		"image/png",
	} {
		t.Run(contentType, func(t *testing.T) {
			t.Parallel()
			rec := serve(t, "gzip", func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", contentType)
				_, _ = w.Write([]byte(want))
			})

			assert.Empty(t, rec.Header().Get("Content-Encoding"))
			assert.Equal(t, want, rec.Body.String())
		})
	}
}

// GetInstanceMetrics and the Bedrock event stream walk the writer chain for
// http.Flusher before writing a header and fail the request if they do not find
// one, so the wrapper must present it and must expose the writer beneath.
func TestWriterPresentsFlusherAndUnwrapsToTheRealWriter(t *testing.T) {
	t.Parallel()
	var sawFlusher, sawUnwrap bool

	rec := serve(t, "gzip", func(w http.ResponseWriter, _ *http.Request) {
		_, sawFlusher = w.(http.Flusher)
		u, ok := w.(interface{ Unwrap() http.ResponseWriter })
		if ok {
			_, sawUnwrap = u.Unwrap().(http.Flusher)
		}
		w.WriteHeader(http.StatusOK)
	})

	assert.True(t, sawFlusher, "handler must find http.Flusher on the wrapper")
	assert.True(t, sawUnwrap, "Unwrap must reach the writer beneath")
	assert.Equal(t, http.StatusOK, rec.Code)
}

// A handler that flushes before the size threshold is streaming, so the bytes
// go out as written rather than waiting for a buffer to fill.
func TestFlushBeforeTheThresholdSendsBytesUncompressed(t *testing.T) {
	t.Parallel()
	const event = "data: {\"cpu\":1}\n\n"

	rec := serve(t, "gzip", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write([]byte(event))
		w.(http.Flusher).Flush()
	})

	assert.Empty(t, rec.Header().Get("Content-Encoding"))
	assert.Equal(t, event, rec.Body.String())
	assert.True(t, rec.Flushed)
}

// A flush after compression has started must push the compressor's buffer, or
// a client waiting on the stream sees nothing until the handler returns.
//
// The bytes are read at the moment of the flush, before the stream is closed,
// so a wrapper that forwards Flush without flushing the compressor fails here.
func TestFlushAfterCompressionStartedReachesTheClient(t *testing.T) {
	t.Parallel()
	want := largeXML()
	var atFlush []byte

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Accept-Encoding", "gzip")

	Middleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/xml")
		_, _ = w.Write([]byte(want))
		w.(http.Flusher).Flush()
		atFlush = append(atFlush, rec.Body.Bytes()...)
	})).ServeHTTP(rec, req)

	require.Equal(t, "gzip", rec.Header().Get("Content-Encoding"))
	require.NotEmpty(t, atFlush, "nothing reached the client at the flush")

	// A flushed deflate stream has no trailer yet, so the reader reports an
	// unexpected EOF after yielding everything written so far.
	zr, err := gzip.NewReader(strings.NewReader(string(atFlush)))
	require.NoError(t, err)
	got, err := io.ReadAll(zr)
	if err != nil {
		require.ErrorIs(t, err, io.ErrUnexpectedEOF)
	}
	assert.Equal(t, want, string(got), "the flush must carry the whole body written so far")
}

// 204 and 304 must carry no body, so nothing may be added to them.
func TestBodylessStatusesAreUntouched(t *testing.T) {
	t.Parallel()

	for _, status := range []int{http.StatusNoContent, http.StatusNotModified} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			t.Parallel()
			rec := serve(t, "gzip", func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(status)
			})

			assert.Equal(t, status, rec.Code)
			assert.Empty(t, rec.Header().Get("Content-Encoding"))
			assert.Empty(t, rec.Body.String())
		})
	}
}

// A handler that writes nothing must still produce a valid response.
func TestEmptyBodyProducesNoEncoding(t *testing.T) {
	t.Parallel()

	rec := serve(t, "gzip", func(http.ResponseWriter, *http.Request) {})

	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Empty(t, rec.Header().Get("Content-Encoding"))
	assert.Empty(t, rec.Body.String())
}

// The status a handler set must survive the header being held back while the
// body's size is measured.
func TestHandlerStatusSurvivesTheDeferredHeader(t *testing.T) {
	t.Parallel()
	want := largeXML()

	rec := serve(t, "gzip", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/xml")
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(want))
	})

	assert.Equal(t, http.StatusAccepted, rec.Code)
	assert.Equal(t, "gzip", rec.Header().Get("Content-Encoding"))
	assert.Equal(t, want, gunzip(t, rec.Body))
}

func TestAcceptsGzip(t *testing.T) {
	t.Parallel()

	for header, want := range map[string]bool{
		"":                      false,
		"gzip":                  true,
		"gzip, deflate, br":     true,
		"br, gzip;q=0.5":        true,
		"*":                     true,
		"deflate":               false,
		"gzip;q=0":              false,
		"gzip;q=0.0":            false,
		"identity":              false,
		"GZIP":                  true,
		"deflate, gzip ; q=0.9": true,
	} {
		t.Run(header, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, want, acceptsGzip(header))
		})
	}
}
