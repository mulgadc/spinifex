package cli_test

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	operatorv1 "github.com/mulgadc/spinifex/contracts/operator/v1"
	gateway_spx "github.com/mulgadc/spinifex/spinifex/gateway/spx"
	"github.com/mulgadc/spinifex/spinifex/operator/cli"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func storageStatusBody(t *testing.T, metas []gateway_spx.MetaNodeStatus) string {
	t.Helper()
	out := gateway_spx.StorageStatusOutput{
		Encoding:  gateway_spx.StorageEncodingOutput{Type: "Reed-Solomon", DataShards: 2, ParityShards: 1},
		MetaNodes: metas,
		BlobNodes: []operatorv1.StorageBlobNode{{ID: 1, Host: "10.0.0.1", Port: 9991}},
	}
	b, err := json.Marshal(out)
	require.NoError(t, err)
	return string(b)
}

// tableRow returns the line of the meta node table whose first column is id.
func tableRow(t *testing.T, rendered, id string) []string {
	t.Helper()
	inMeta := false
	for line := range strings.SplitSeq(rendered, "\n") {
		switch {
		case line == "Meta Nodes":
			inMeta = true
		case line == "Blob Nodes":
			inMeta = false
		case inMeta:
			if fields := strings.Fields(line); len(fields) > 0 && fields[0] == id {
				return fields
			}
		}
	}
	t.Fatalf("no meta node row %s in:\n%s", id, rendered)
	return nil
}

// The command must send the console's own request, the signed spinifex
// GetStorageStatus action, and print each meta node's raft state from it.
func TestStorageStatusRendersMetaNodeRaftState(t *testing.T) {
	t.Setenv("AWS_ACCESS_KEY_ID", "AKIAEXAMPLE")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY")

	response := storageStatusBody(t, []gateway_spx.MetaNodeStatus{
		{ID: 1, Host: "10.0.0.1", Port: 6660, Healthy: true, State: "Leader", Leader: "1", Term: "4", CommitIdx: "120", AppliedIdx: "119", IsLeader: true},
		{ID: 2, Host: "10.0.0.2", Port: 6660, Healthy: true, State: "Follower", Leader: "1", Term: "4", CommitIdx: "120", AppliedIdx: "120"},
		{ID: 3, Host: "10.0.0.3", Port: 6660},
	})
	var got http.Request
	var gotBody string
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		got, gotBody = *r, string(b)
		_, _ = w.Write([]byte(response))
	}))
	t.Cleanup(srv.Close)

	var buf bytes.Buffer
	err := cli.GetStorageStatusRemote(t.Context(), cli.AdminTarget(srv.URL+"/", "us-west-1", writeServerCA(t, srv)), &buf)
	require.NoError(t, err)

	assert.Equal(t, http.MethodPost, got.Method)
	assert.Equal(t, "/", got.URL.Path)
	assert.Equal(t, "Action=GetStorageStatus", gotBody)
	assert.Contains(t, got.Header.Get("Authorization"), "/us-west-1/spinifex/aws4_request")

	out := buf.String()
	assert.Contains(t, out, "Reed-Solomon RS(2,1)")
	assert.Contains(t, out, "Degraded", "an unreachable meta node degrades the cluster")
	assert.Equal(t, []string{"1", "10.0.0.1", "6660", "Healthy", "Leader", "1", "4", "120", "119"}, tableRow(t, out, "1"))
	assert.Equal(t, []string{"2", "10.0.0.2", "6660", "Healthy", "Follower", "1", "4", "120", "120"}, tableRow(t, out, "2"))
	assert.Equal(t, []string{"3", "10.0.0.3", "6660", "Unreachable", "-", "-", "-", "-", "-"}, tableRow(t, out, "3"))
}

// Cluster health follows the console: every node must answer, and every node
// with raft state must agree on one known leader.
func TestStorageStatusClusterHealth(t *testing.T) {
	t.Setenv("AWS_ACCESS_KEY_ID", "AKIAEXAMPLE")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY")

	tests := []struct {
		name  string
		metas []gateway_spx.MetaNodeStatus
		want  string
	}{
		{"no meta nodes", nil, "Unknown"},
		{"one agreed leader", []gateway_spx.MetaNodeStatus{
			{ID: 1, Healthy: true, State: "Leader", Leader: "1"},
			{ID: 2, Healthy: true, State: "Follower", Leader: "1"},
		}, "Healthy"},
		{"readyz-only nodes carry no raft state", []gateway_spx.MetaNodeStatus{
			{ID: 1, Healthy: true}, {ID: 2, Healthy: true},
		}, "Healthy"},
		{"split leaders", []gateway_spx.MetaNodeStatus{
			{ID: 1, Healthy: true, State: "Leader", Leader: "1"},
			{ID: 2, Healthy: true, State: "Leader", Leader: "2"},
		}, "Degraded"},
		{"leaderless candidate", []gateway_spx.MetaNodeStatus{
			{ID: 1, Healthy: true, State: "Candidate"},
		}, "Degraded"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var got http.Request
			srv := newAdminTestServer(t, http.StatusOK, storageStatusBody(t, tc.metas), &got)
			var buf bytes.Buffer
			require.NoError(t, cli.GetStorageStatusRemote(t.Context(), cli.AdminTarget(srv.URL, "us-west-1", writeServerCA(t, srv)), &buf))
			assert.Contains(t, buf.String(), "Cluster Health:  "+tc.want+"\n")
		})
	}
}

// A gateway refusal arrives in the EC2 XML envelope; its code must reach the
// operator rather than a JSON decode error.
func TestStorageStatusSurfacesGatewayError(t *testing.T) {
	t.Setenv("AWS_ACCESS_KEY_ID", "AKIAEXAMPLE")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY")

	var got http.Request
	srv := newAdminTestServer(t, http.StatusForbidden,
		`<?xml version="1.0" encoding="UTF-8"?><Response><Errors><Error><Code>AccessDenied</Code><Message>no</Message></Error></Errors><RequestID>req-9</RequestID></Response>`, &got)

	err := cli.GetStorageStatusRemote(t.Context(), cli.AdminTarget(srv.URL, "us-west-1", writeServerCA(t, srv)), io.Discard)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "AccessDenied")
	assert.Contains(t, err.Error(), "req-9")
}

// A failed call is a runtime failure, not a usage mistake: cobra must print
// neither usage nor the error, which Execute already reports once.
func TestStorageStatusCommandFailureIsNotAUsageError(t *testing.T) {
	t.Setenv("AWS_ACCESS_KEY_ID", "AKIAEXAMPLE")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY")

	var got http.Request
	srv := newAdminTestServer(t, http.StatusForbidden,
		`<?xml version="1.0" encoding="UTF-8"?><Response><Errors><Error><Code>AccessDenied</Code><Message>no</Message></Error></Errors><RequestID>req-9</RequestID></Response>`, &got)

	var stderr bytes.Buffer
	err := cli.ExecuteRoot([]string{"admin", "storage", "status",
		"--endpoint", srv.URL, "--region", "us-west-1", "--ca-bundle", writeServerCA(t, srv)}, &stderr)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "AccessDenied")
	assert.Equal(t, http.MethodPost, got.Method)
	assert.Empty(t, stderr.String())
}
