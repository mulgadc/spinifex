// Package compress negotiates response content encoding for the AWS-compatible
// gateway.
//
// Responses here are XML and JSON describing repeated records, which compress
// by an order of magnitude or more. A browser always offers gzip and cannot be
// configured not to, so the console benefits on every call.
//
// Compression is a transport property. It is not a substitute for a response
// whose size is bounded by what it describes rather than by how many exist: a
// high ratio is evidence of redundancy, not of efficiency.
package compress

import (
	"compress/gzip"
	"net/http"
	"strings"
	"sync"
)

// minCompressBytes is the smallest response worth compressing. Below about a
// kilobyte the gzip header and the CPU cost outweigh the saving, and a short
// AWS error envelope is the commonest reply on the surface.
const minCompressBytes = 1024

// gzipLevel trades ratio for CPU. The default sits close to the best ratio on
// the repetitive XML this surface returns.
const gzipLevel = gzip.DefaultCompression

// passThroughTypes are content types that must reach the client byte for byte.
//
// The first two stream: the handler writes an event and flushes, so buffering
// to reach the size threshold would hold a sample until the next one arrived.
// The rest are already compressed, so a second pass costs CPU and grows them.
var passThroughTypes = []string{
	"text/event-stream",
	"application/vnd.amazon.eventstream",
	"application/vnd.oci.",
	"application/vnd.docker.",
	"application/gzip",
	"application/zip",
	"application/x-gzip",
	"image/",
	"video/",
	"audio/",
}

var writerPool = sync.Pool{
	New: func() any {
		w, _ := gzip.NewWriterLevel(nil, gzipLevel)
		return w
	},
}

// Middleware compresses a response when the client offered gzip, the body is
// large enough to be worth it, and the handler has not ruled it out.
//
// Mount it inside the authenticated group. The OCI Distribution registry sets
// its own Content-Length and serves already-compressed layers, so it is
// excluded by where this is mounted as well as by the checks below.
func Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !acceptsGzip(r.Header.Get("Accept-Encoding")) {
			next.ServeHTTP(w, r)
			return
		}

		cw := &compressWriter{ResponseWriter: w}
		defer cw.close()
		next.ServeHTTP(cw, r)
	})
}

// acceptsGzip reports whether the header offers gzip without refusing it.
//
// A q-value of 0 is a refusal, which is how a client opts out. Anything else,
// including an absent q-value, is an offer.
func acceptsGzip(header string) bool {
	for part := range strings.SplitSeq(header, ",") {
		fields := strings.Split(strings.TrimSpace(part), ";")
		coding := strings.ToLower(strings.TrimSpace(fields[0]))
		if coding != "gzip" && coding != "*" {
			continue
		}
		for _, param := range fields[1:] {
			param = strings.ToLower(strings.ReplaceAll(param, " ", ""))
			if param == "q=0" || strings.HasPrefix(param, "q=0.0") {
				return false
			}
		}
		return true
	}
	return false
}

// shouldCompress reports whether a response with these headers may be gzipped.
func shouldCompress(h http.Header) bool {
	// Already encoded, so re-encoding would corrupt it.
	if h.Get("Content-Encoding") != "" {
		return false
	}

	// A handler that declared its own length has told the client exactly how
	// many bytes to expect. Compressing would make that number wrong, and the
	// OCI Distribution clients enforce it.
	if h.Get("Content-Length") != "" {
		return false
	}

	contentType := strings.ToLower(h.Get("Content-Type"))
	for _, t := range passThroughTypes {
		if strings.HasPrefix(contentType, t) {
			return false
		}
	}
	return true
}

// compressWriter buffers the first minCompressBytes to decide whether the body
// is worth compressing, then either gzips the rest or passes everything
// through. A Flush before that decision means the handler is streaming, so
// compression is abandoned rather than holding the data back.
type compressWriter struct {
	http.ResponseWriter

	status      int
	wroteHeader bool
	// decided is set once the choice between gzip and pass-through is made.
	decided    bool
	compressed bool
	buf        []byte
	gz         *gzip.Writer
}

// Unwrap exposes the underlying writer, so a handler probing the chain for
// http.Flusher or http.Hijacker still finds it.
func (cw *compressWriter) Unwrap() http.ResponseWriter {
	return cw.ResponseWriter
}

func (cw *compressWriter) WriteHeader(status int) {
	if cw.wroteHeader {
		return
	}
	cw.wroteHeader = true
	cw.status = status

	// A header-only reply, an error envelope or a redirect carries nothing worth
	// compressing, and 204 and 304 must carry no body at all.
	if !shouldCompress(cw.Header()) || status == http.StatusNoContent || status == http.StatusNotModified {
		cw.decide(false)
		return
	}

	// The header is held back until the size is known: Content-Encoding cannot
	// be added after the status line has gone out.
}

func (cw *compressWriter) Write(p []byte) (int, error) {
	if !cw.wroteHeader {
		cw.WriteHeader(http.StatusOK)
	}

	if cw.decided {
		if cw.compressed {
			return cw.gz.Write(p)
		}
		return cw.ResponseWriter.Write(p)
	}

	cw.buf = append(cw.buf, p...)
	if len(cw.buf) >= minCompressBytes {
		cw.decide(true)
	}
	return len(p), nil
}

// Flush is required by the SSE and event-stream handlers, which probe the
// writer chain for it before writing a header and fail the request if it is
// absent.
//
// A flush before the size decision means the handler is streaming, so the
// buffered bytes go out uncompressed and nothing is held back.
func (cw *compressWriter) Flush() {
	if !cw.decided {
		cw.decide(false)
	}
	if cw.compressed {
		// Without this the compressor keeps the data, and a stream the client is
		// waiting on stalls rather than arriving late.
		_ = cw.gz.Flush()
	}
	if f, ok := cw.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// decide commits to gzip or pass-through, writes the held-back header and
// drains the buffer.
func (cw *compressWriter) decide(compressed bool) {
	if cw.decided {
		return
	}
	cw.decided = true
	cw.compressed = compressed

	if !cw.wroteHeader {
		cw.wroteHeader = true
		cw.status = http.StatusOK
	}

	if compressed {
		cw.Header().Set("Content-Encoding", "gzip")
		cw.Header().Add("Vary", "Accept-Encoding")
		// The length of the compressed body is not known yet, and the value the
		// handler may have set describes the uncompressed one.
		cw.Header().Del("Content-Length")
		cw.ResponseWriter.WriteHeader(cw.status)

		gz, _ := writerPool.Get().(*gzip.Writer)
		gz.Reset(cw.ResponseWriter)
		cw.gz = gz
		if len(cw.buf) > 0 {
			_, _ = gz.Write(cw.buf)
		}
		cw.buf = nil
		return
	}

	if cw.status != 0 {
		cw.ResponseWriter.WriteHeader(cw.status)
	}
	if len(cw.buf) > 0 {
		_, _ = cw.ResponseWriter.Write(cw.buf)
		cw.buf = nil
	}
}

// close finishes the gzip stream, or emits a body that never reached the size
// threshold.
func (cw *compressWriter) close() {
	if !cw.decided {
		cw.decide(false)
		return
	}
	if cw.compressed && cw.gz != nil {
		_ = cw.gz.Close()
		writerPool.Put(cw.gz)
		cw.gz = nil
	}
}
