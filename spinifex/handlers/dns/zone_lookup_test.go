package dns

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/mulgadc/spinifex/spinifex/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// minimalZoneTOML renders just enough of the zone TOML format for
// nsconfig.ReadZoneRaw to parse successfully — HostsZone only cares that the
// object exists and decodes, not its record contents.
func minimalZoneTOML(domain string) string {
	return fmt.Sprintf(`version = 1.0
[domain]
domain = %q
active = true
soa = "ns1.%s."
[defaults]
ttl = 300
type = 1
class = 1
`, domain, domain)
}

// fakeZoneS3 is a mutable path-style S3 endpoint (GET only) backing HostsZone
// lookups. Keys listed in errorKeys return 403 instead of the usual 200/404,
// simulating a read failure for that zone object — 403 rather than a retried
// (and so slow) 5xx, since HostsZone branches only on err != nil. requested
// records every key fetched, letting a test assert which candidate zones were
// actually queried.
func fakeZoneS3(t *testing.T, bucket string, objects map[string]string, errorKeys map[string]bool) (endpoint string, requested *[]string) {
	t.Helper()
	var mu sync.Mutex
	var log []string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := strings.TrimPrefix(r.URL.Path, "/"+bucket+"/")
		mu.Lock()
		log = append(log, key)
		mu.Unlock()
		if errorKeys[key] {
			http.Error(w, "access denied", http.StatusForbidden)
			return
		}
		body, ok := objects[key]
		if !ok {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Length", strconv.Itoa(len(body)))
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv.URL, &log
}

// hostsZoneTestConfig builds a *config.Config wired at endpoint with the given
// hosted zone objects and per-key error injection, mirroring newTestWriter's
// northstar.toml construction.
func hostsZoneTestConfig(t *testing.T, objects map[string]string, errorKeys map[string]bool) (*config.Config, *[]string) {
	t.Helper()
	endpoint, requested := fakeZoneS3(t, "northstar", objects, errorKeys)
	tomlBody := fmt.Sprintf(`listen = "0.0.0.0:5300"
default_domain = "spx3.net"
[s3]
endpoint = %q
bucket = "northstar"
region = "us-east-1"
access_key = "READONLY"
secret_key = "READONLY"
`, endpoint)
	configPath := filepath.Join(t.TempDir(), "northstar.toml")
	require.NoError(t, os.WriteFile(configPath, []byte(tomlBody), 0o600))

	cfg := &config.Config{
		Predastore: config.PredastoreConfig{AccessKey: "SYSTEM", SecretKey: "SYSTEMSECRET"},
		Northstar:  config.NorthstarConfig{ConfigPath: configPath},
	}
	return cfg, requested
}

func TestHostsZone(t *testing.T) {
	objects := map[string]string{
		"lab.example.com.toml": minimalZoneTOML("lab.example.com"),
	}
	cfg, _ := hostsZoneTestConfig(t, objects, nil)

	cases := []struct {
		name   string
		domain string
		want   bool
	}{
		{"exact match on the hosted zone", "lab.example.com", true},
		{"subdomain walks up to the hosted parent zone", "jellyfin.lab.example.com", true},
		{"deep subdomain walks up to the hosted parent zone", "a.b.lab.example.com", true},
		{"domain outside every hosted zone", "unrelated.example.org", false},
		{"superstring, not a subdomain, must not string-suffix match", "notlab.example.com", false},
		{"trailing dot and mixed case are normalised", "JELLYFIN.LAB.EXAMPLE.COM.", true},
		{"empty domain", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, HostsZone(cfg, tc.domain))
		})
	}
}

// TestHostsZone_NeverQueriesBareTLD asserts the walk stops once fewer than two
// labels remain, so a lookup for "example.com" (or anything under it) never
// issues a GET for "com.toml".
func TestHostsZone_NeverQueriesBareTLD(t *testing.T) {
	cfg, requested := hostsZoneTestConfig(t, map[string]string{}, nil)

	got := HostsZone(cfg, "sub.example.com")
	assert.False(t, got)
	for _, key := range *requested {
		assert.NotEqual(t, "com.toml", key, "must never query the bare TLD as a candidate zone")
	}
}

// TestHostsZone_S3ErrorAtOneLevelContinuesToParent asserts a read error on the
// most specific candidate does not abort the walk — the error is logged and
// treated as "not hosted" for that candidate only, so a real parent zone one
// level up is still found.
func TestHostsZone_S3ErrorAtOneLevelContinuesToParent(t *testing.T) {
	t.Parallel()
	objects := map[string]string{
		"example.com.toml": minimalZoneTOML("example.com"),
	}
	errorKeys := map[string]bool{"lab.example.com.toml": true}
	cfg, _ := hostsZoneTestConfig(t, objects, errorKeys)

	assert.True(t, HostsZone(cfg, "lab.example.com"), "a transient error on one candidate must not hide a real parent zone")
}

// TestHostsZone_S3ErrorEverywhereReturnsFalse asserts a lookup failure with no
// good answer anywhere degrades to "not hosted" rather than propagating an
// error — a certificate request must not hard-fail because a zone lookup was
// unavailable.
func TestHostsZone_S3ErrorEverywhereReturnsFalse(t *testing.T) {
	t.Parallel()
	errorKeys := map[string]bool{"lab.example.com.toml": true, "example.com.toml": true}
	cfg, _ := hostsZoneTestConfig(t, map[string]string{}, errorKeys)

	assert.False(t, HostsZone(cfg, "lab.example.com"))
}

// TestHostsZone_NorthstarNotConfigured asserts an unconfigured (or
// misconfigured) northstar S3 backend is "not hosted", not an error — the same
// degrade-safe default zoneS3Config's other callers (Writer, Reconciler) use.
func TestHostsZone_NorthstarNotConfigured(t *testing.T) {
	assert.False(t, HostsZone(&config.Config{}, "lab.example.com"))
	assert.False(t, HostsZone(nil, "lab.example.com"))
}
