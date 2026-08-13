package handlers_imds

import (
	"errors"
	"log/slog"
	"net/http"
	"os"
	"sync"
	"time"
)

// pathSpinifexCACert serves the deployment CA. It sits outside the /latest tree
// because it is a Spinifex extension: the AWS-compatible metadata surface must
// stay byte-identical to EC2's, so no new key is added under /latest/meta-data.
const pathSpinifexCACert = "/spinifex/ca.pem"

// caCertContentType matches the console's /api/ca.pem endpoint.
const caCertContentType = "application/x-pem-file"

// caCertCache serves the deployment CA from disk, re-reading only when the file
// changes so a CA rotation is picked up without restarting vpcd.
type caCertCache struct {
	path string

	mu      sync.Mutex
	pem     []byte
	modTime time.Time
	size    int64
}

func newCACertCache(path string) *caCertCache {
	return &caCertCache{path: path}
}

// load returns the current CA PEM, re-reading when modtime or size moved.
func (c *caCertCache) load() ([]byte, error) {
	if c.path == "" {
		return nil, errors.New("no CA certificate path configured")
	}
	fi, err := os.Stat(c.path)
	if err != nil {
		return nil, err
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	if c.pem != nil && fi.ModTime().Equal(c.modTime) && fi.Size() == c.size {
		return c.pem, nil
	}
	pem, err := os.ReadFile(c.path)
	if err != nil {
		return nil, err
	}
	c.pem, c.modTime, c.size = pem, fi.ModTime(), fi.Size()
	return c.pem, nil
}

// handleCACert serves the deployment CA so a guest can install it into its own
// trust store. Deliberately token-free: a CA certificate is public material,
// already downloadable unauthenticated from the console, and requiring an
// IMDSv2 token would push the PUT handshake into every bootstrap snippet.
// rejectForwarded still wraps the mux, so the SSRF guard is unchanged, and a
// relayed request would yield a public certificate and no credential.
func (s *IMDSServiceImpl) handleCACert(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}

	eni := s.resolveCaller(r)
	if eni == nil {
		w.WriteHeader(http.StatusNotFound)
		return
	}

	pem, err := s.caCert.load()
	if err != nil {
		slog.WarnContext(r.Context(), "IMDS: deployment CA unavailable", "instance_id", eni.instanceID, "err", err)
		w.WriteHeader(http.StatusNotFound)
		return
	}

	slog.InfoContext(r.Context(), "IMDS: serving deployment CA", "instance_id", eni.instanceID, "private_ip", eni.privateIP)
	w.Header().Set("Content-Type", caCertContentType)
	_, _ = w.Write(pem)
}
