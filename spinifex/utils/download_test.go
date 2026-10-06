package utils

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/pterm/pterm"
	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"
)

// captureFD1 redirects file descriptor 1 for the duration of fn, so it also
// catches writers that captured os.Stdout at init (pterm's cursor and colour).
func captureFD1(t *testing.T, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	require.NoError(t, err)
	saved, err := unix.Dup(1)
	require.NoError(t, err)

	done := make(chan []byte)
	go func() {
		b, _ := io.ReadAll(r)
		done <- b
	}()

	func() {
		defer func() {
			require.NoError(t, unix.Dup2(saved, 1))
			_ = unix.Close(saved)
			_ = w.Close()
		}()
		require.NoError(t, unix.Dup2(int(w.Fd()), 1))
		fn()
	}()

	out := <-done
	_ = r.Close()
	return string(out)
}

func TestDownloadFileWithProgressPlainWhenStylingDisabled(t *testing.T) {
	wasRaw := pterm.RawOutput
	t.Cleanup(func() {
		if wasRaw {
			pterm.DisableStyling()
		} else {
			pterm.EnableStyling()
		}
	})
	pterm.DisableStyling()

	const chunk = 32 * 1024
	const chunks = 64
	payload := bytes.Repeat([]byte("spinifex"), chunk*chunks/8)

	tests := []struct {
		name          string
		contentLength bool
		wantStart     string
	}{
		{name: "content length", contentLength: true, wantStart: "Downloading img (2.0 MiB) ..."},
		{name: "chunked without content length", contentLength: false, wantStart: "Downloading img (size unknown) ..."},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				if tt.contentLength {
					w.Header().Set("Content-Length", strconv.Itoa(len(payload)))
				}
				flusher, ok := w.(http.Flusher)
				if !ok {
					t.Error("response writer cannot flush")
					return
				}
				for off := 0; off < len(payload); off += chunk {
					_, _ = w.Write(payload[off : off+chunk])
					flusher.Flush()
				}
			}))
			defer srv.Close()

			dest := filepath.Join(t.TempDir(), "img.raw")
			var dlErr error
			out := captureFD1(t, func() {
				dlErr = DownloadFileWithProgress(srv.URL, "img", dest, 0)
			})
			require.NoError(t, dlErr)

			got, err := os.ReadFile(dest)
			require.NoError(t, err)
			require.True(t, bytes.Equal(payload, got), "downloaded file differs from served payload")

			require.NotContains(t, out, "\x1b", "plain output carries an escape sequence")
			require.NotContains(t, out, "\r", "plain output redraws a line")
			lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
			require.Equal(t, []string{tt.wantStart, "Downloaded img (2.0 MiB)"}, lines)
		})
	}
}
