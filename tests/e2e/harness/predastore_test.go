//go:build e2e

package harness

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go/aws/awserr"
)

// readyzStub serves one canned /readyz response.
func readyzStub(t *testing.T, status int, body string) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv.URL + "/readyz"
}

// TestPredastoreReadyFailures_NamesTheUnreachableBlobNode is the check the
// backend-outage test was missing. A gate that has lost a blob peer still
// reconstructs every read, so the round trip passes; the failed check name is
// the only place the missing peer is visible.
func TestPredastoreReadyFailures_NamesTheUnreachableBlobNode(t *testing.T) {
	url := readyzStub(t, http.StatusServiceUnavailable,
		`{"status":"unready","checks":{"meta_leader":"ok","blob_nodes":"failed","blob_node_5":"failed","blob_node_6":"ok"}}`)

	failed, err := predastoreReadyFailures(context.Background(), url)
	if err != nil {
		t.Fatalf("predastoreReadyFailures: %v", err)
	}
	if got, want := strings.Join(failed, ","), "blob_node_5=failed,blob_nodes=failed"; got != want {
		t.Errorf("failed checks = %q, want %q", got, want)
	}
}

func TestPredastoreReadyFailures_ReadyReportsNothing(t *testing.T) {
	url := readyzStub(t, http.StatusOK,
		`{"status":"ready","checks":{"meta_leader":"ok","blob_nodes":"ok","blob_node_5":"ok"}}`)

	failed, err := predastoreReadyFailures(context.Background(), url)
	if err != nil {
		t.Fatalf("predastoreReadyFailures: %v", err)
	}
	if len(failed) != 0 {
		t.Errorf("failed checks = %v, want none", failed)
	}
}

// An unready answer carrying no checks tells us nothing about why, and
// reporting it as "no failures" would be the probe hiding the finding.
func TestPredastoreReadyFailures_UnreadyWithNoChecksIsAnError(t *testing.T) {
	url := readyzStub(t, http.StatusServiceUnavailable, `{"status":"unready","checks":{}}`)

	if _, err := predastoreReadyFailures(context.Background(), url); err == nil {
		t.Error("an unready answer with no checks must not read as no failures")
	}
}

// A host that serves no admin listener is a configuration state, not a
// failure, so the caller has to be able to tell the two apart.
func TestPredastoreReadyFailures_NoListenerIsAnError(t *testing.T) {
	if _, err := predastoreReadyFailures(context.Background(), "http://127.0.0.1:1/readyz"); err == nil {
		t.Error("a host with no admin listener must report an error, not readiness")
	}
}

// TestIsIncorrectVolumeState covers the one refusal teardown tolerates. A
// volume already on its way to available is the outcome the detach wanted, and
// failing on it fails a test whose assertions all passed.
func TestIsIncorrectVolumeState(t *testing.T) {
	if !isIncorrectVolumeState(awserr.New("IncorrectState", "not attached", nil)) {
		t.Error("a volume that is not attached must read as already released")
	}
	if isIncorrectVolumeState(awserr.New("VolumeInUse", "attached elsewhere", nil)) {
		t.Error("VolumeInUse is a real failure and must not be swallowed")
	}
	if isIncorrectVolumeState(errors.New("connection refused")) {
		t.Error("a non-AWS error must not read as already released")
	}
}
