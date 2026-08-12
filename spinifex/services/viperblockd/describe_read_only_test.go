package viperblockd

import (
	"crypto/rand"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/mulgadc/bluebottle/pkg/masterkey"
	"github.com/mulgadc/viperblock/viperblock"
	"github.com/mulgadc/viperblock/viperblock/backends/file"
	"github.com/stretchr/testify/require"
)

const describeTestVolumeSize = 64 << 20

// recordingS3 serves one volume's config.json and records every request it is
// asked for, so what a describe costs is asserted from the wire.
type recordingS3 struct {
	*httptest.Server

	mu       sync.Mutex
	requests []string
	conns    map[net.Conn]struct{}
}

func newRecordingS3(t *testing.T, configPath string, config []byte) *recordingS3 {
	t.Helper()
	r := &recordingS3{conns: map[net.Conn]struct{}{}}
	r.Server = httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		r.mu.Lock()
		r.requests = append(r.requests, req.Method+" "+req.URL.RequestURI())
		r.mu.Unlock()

		if req.Method == http.MethodGet && strings.HasSuffix(req.URL.Path, configPath) {
			_, _ = w.Write(config)
			return
		}
		// Lists and writes are answered rather than refused so a describe that
		// issues them gets far enough for every assertion below to be reached.
		// Refusing them would fail this test on the first one and leave the
		// rest unproven.
		if req.URL.Query().Get("list-type") == "2" {
			w.Header().Set("Content-Type", "application/xml")
			_, _ = w.Write([]byte(`<?xml version="1.0" encoding="UTF-8"?><ListBucketResult xmlns="http://s3.amazonaws.com/doc/2006-03-01/"><Name>test-bucket</Name><KeyCount>0</KeyCount><IsTruncated>false</IsTruncated></ListBucketResult>`))
			return
		}
		if req.Method == http.MethodPut {
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	r.Config.ConnState = func(c net.Conn, state http.ConnState) {
		if state != http.StateNew {
			return
		}
		r.mu.Lock()
		r.conns[c] = struct{}{}
		r.mu.Unlock()
	}
	r.Start()
	t.Cleanup(r.Close)
	return r
}

func (r *recordingS3) connCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.conns)
}

func (r *recordingS3) matching(pred func(string) bool) []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []string
	for _, req := range r.requests {
		if pred(req) {
			out = append(out, req)
		}
	}
	return out
}

// seedVolumeConfig writes a real encrypted volume's state with a real engine
// and returns the persisted config.json. Hand-rolling the envelope would test
// the test's idea of the format rather than viperblock's.
func seedVolumeConfig(t *testing.T, volume string, key *masterkey.Key) []byte {
	t.Helper()
	store := t.TempDir()
	vb, err := viperblock.New(&viperblock.VB{
		VolumeName:        volume,
		VolumeSize:        describeTestVolumeSize,
		BaseDir:           t.TempDir(),
		MasterKey:         key,
		EncryptionEnabled: true,
	}, "file", file.FileConfig{BaseDir: store, VolumeName: volume})
	require.NoError(t, err)
	defer func() {
		vb.StopChunkUploader()
		vb.StopWALSyncer()
	}()
	require.NoError(t, vb.Backend.Init())
	require.NoError(t, vb.SaveState())

	config, err := os.ReadFile(filepath.Join(store, volume, "config.json"))
	require.NoError(t, err)
	return config
}

func testMasterKey(t *testing.T) *masterkey.Key {
	t.Helper()
	raw := make([]byte, 32)
	_, err := rand.Read(raw)
	require.NoError(t, err)
	key, err := masterkey.New(raw)
	require.NoError(t, err)
	return key
}

// TestDescribeVolume_ReadsWithoutOpeningTheVolume pins what a describe is
// allowed to cost. Opening an engine to answer it paid a reachability list and,
// on an encrypted volume, a PutObject of the volume's own config.json — a
// write issued by whichever worker took the request rather than by the node
// that owns the volume.
func TestDescribeVolume_ReadsWithoutOpeningTheVolume(t *testing.T) {
	const volume = "vol-describeonly001"
	key := testMasterKey(t)
	server := newRecordingS3(t, "/"+volume+"/config.json", seedVolumeConfig(t, volume, key))

	_, natsURL := setupEmbeddedNATS(t)
	cfg := setupTestConfig(t, natsURL)
	cfg.S3Host = server.URL
	cfg.masterKey = key

	vol, err := describeVolumeEngine(t.Context(), cfg, volume)
	require.NoError(t, err)
	require.Equal(t, int64(describeTestVolumeSize), vol.CapacityBytes)

	require.Empty(t, server.matching(func(r string) bool { return strings.HasPrefix(r, "PUT ") }),
		"a describe wrote to the volume it was only asked about")
	require.Empty(t, server.matching(func(r string) bool { return strings.Contains(r, "list-type=2") }),
		"a describe paid for a reachability list it did not need")
}

// TestDescribeVolume_ReusesOneConnection is what makes the single request a
// describe now issues cheap. Each viperblock backend builds its own HTTP client
// unless handed one, and a client per describe is a connection pool per
// describe — so the one request pays for the handshake every time.
func TestDescribeVolume_ReusesOneConnection(t *testing.T) {
	const volume = "vol-describeconn001"
	key := testMasterKey(t)
	server := newRecordingS3(t, "/"+volume+"/config.json", seedVolumeConfig(t, volume, key))

	_, natsURL := setupEmbeddedNATS(t)
	cfg := setupTestConfig(t, natsURL)
	cfg.S3Host = server.URL
	cfg.masterKey = key

	for range 3 {
		_, err := describeVolumeEngine(t.Context(), cfg, volume)
		require.NoError(t, err)
	}

	require.Equal(t, 1, server.connCount(),
		"three describes opened three connections, so each one paid its own setup")
}
