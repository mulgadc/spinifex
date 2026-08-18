package cmd_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mulgadc/spinifex/cmd/spinifex/cmd"
	"github.com/mulgadc/spinifex/spinifex/gateway"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// adminTestCredentials puts a static key in the environment so the SigV4 signer
// resolves without reaching for IMDS.
func adminTestCredentials(t *testing.T) {
	t.Helper()
	t.Setenv("AWS_ACCESS_KEY_ID", "AKIAEXAMPLE")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY")
}

// newScriptedAdminServer answers each successive request with the next body,
// repeating the last one, which is how a job that changes state between polls
// is reproduced.
func newScriptedAdminServer(t *testing.T, bodies ...string) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var calls atomic.Int32

	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		index := int(calls.Add(1)) - 1
		if index >= len(bodies) {
			index = len(bodies) - 1
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(bodies[index]))
	}))
	t.Cleanup(srv.Close)
	return srv, &calls
}

// The delete request must reach /admin/DeleteAccount signed for the spinifex
// service: a key signed for another service is treated as probing and denied.
func TestDeleteAccountRemoteSignsAndPosts(t *testing.T) {
	adminTestCredentials(t)

	var got http.Request
	srv := newAdminTestServer(t, http.StatusOK,
		`{"deletionId":"d-1","accountId":"000000000042","state":"RUNNING"}`, &got)

	out, err := cmd.DeleteAccountRemote(t.Context(),
		cmd.AdminTarget(srv.URL, "us-west-1", writeServerCA(t, srv)),
		gateway.DeleteAccountRequest{
			AccountID:   "000000000042",
			AccountName: "tenant@example.com",
			ClientToken: strings.Repeat("a", 32),
		})
	require.NoError(t, err)

	assert.Equal(t, "d-1", out.DeletionID)
	assert.Equal(t, gateway.DeletionStateRunning, out.State)

	assert.Equal(t, http.MethodPost, got.Method)
	assert.Equal(t, "/admin/DeleteAccount", got.URL.Path)
	assert.Contains(t, got.Header.Get("Authorization"), "/us-west-1/spinifex/aws4_request")
}

// A dry run answers inline with the inventory, which is what the operator
// confirms against before the real call.
func TestDeleteAccountRemoteDryRunCarriesTheInventory(t *testing.T) {
	adminTestCredentials(t)

	var got http.Request
	srv := newAdminTestServer(t, http.StatusOK,
		`{"accountId":"000000000042","state":"DRY_RUN","dryRun":true,"inventory":{"accountId":"000000000042",`+
			`"dryRun":true,"stages":[{"stage":1,"deleted":[{"kind":"instance","id":"i-1"}],"elapsed":"0s"}]}}`, &got)

	out, err := cmd.DeleteAccountRemote(t.Context(),
		cmd.AdminTarget(srv.URL, "us-west-1", writeServerCA(t, srv)),
		gateway.DeleteAccountRequest{AccountID: "000000000042", DryRun: true})
	require.NoError(t, err)

	assert.True(t, out.DryRun)
	require.NotNil(t, out.Inventory)
	assert.Equal(t, 1, out.Inventory.DeletedCount())
}

// A refusal must surface as the gateway's code, not as a decode failure, so the
// operator can tell a protected account from an unreachable cluster.
func TestDeleteAccountRemoteSurfacesGatewayError(t *testing.T) {
	adminTestCredentials(t)

	var got http.Request
	srv := newAdminTestServer(t, http.StatusForbidden,
		`{"error":{"code":"AccessDenied","message":"denied"},"requestId":"req-9"}`, &got)

	_, err := cmd.DeleteAccountRemote(t.Context(),
		cmd.AdminTarget(srv.URL, "us-west-1", writeServerCA(t, srv)),
		gateway.DeleteAccountRequest{
			AccountID:   "000000000001",
			AccountName: "spinifex",
			ClientToken: strings.Repeat("a", 32),
		})

	require.Error(t, err)
	assert.Contains(t, err.Error(), "AccessDenied")
	assert.Contains(t, err.Error(), "req-9")
	assert.False(t, cmd.RetryableAdminError(err), "a protected account is not fixed by retrying")
}

func TestDescribeAccountDeletionRemoteReportsProgress(t *testing.T) {
	adminTestCredentials(t)

	var got http.Request
	srv := newAdminTestServer(t, http.StatusOK,
		`{"deletionId":"d-1","state":"RUNNING","stages":[{"stage":1,"elapsed":"5s"}]}`, &got)

	out, err := cmd.DescribeAccountDeletionRemote(t.Context(),
		cmd.AdminTarget(srv.URL, "us-west-1", writeServerCA(t, srv)), "000000000042")
	require.NoError(t, err)

	assert.Equal(t, "/admin/DescribeAccountDeletion", got.URL.Path)
	assert.Equal(t, gateway.DeletionStateRunning, out.State)
	assert.Len(t, out.Stages, 1)
}

// The follow loop is what makes --remote report the same outcome the local
// path prints. It must keep polling while the job runs and stop on the first
// terminal state.
func TestFollowAccountDeletionPollsUntilComplete(t *testing.T) {
	adminTestCredentials(t)
	defer cmd.SetAccountDeletePollInterval(time.Millisecond)()

	srv, calls := newScriptedAdminServer(t,
		`{"deletionId":"d-1","state":"RUNNING","stages":[{"stage":1,"elapsed":"5s"}]}`,
		`{"deletionId":"d-1","state":"RUNNING","stages":[{"stage":1,"elapsed":"5s"},{"stage":2,"elapsed":"1s"}]}`,
		`{"deletionId":"d-1","state":"COMPLETED","stages":[{"stage":1,"elapsed":"5s"},{"stage":2,"elapsed":"1s"}]}`,
	)

	err := cmd.FollowAccountDeletion(t.Context(),
		cmd.AdminTarget(srv.URL, "us-west-1", writeServerCA(t, srv)), "000000000042")

	require.NoError(t, err)
	assert.Equal(t, int32(3), calls.Load(), "the loop must keep polling while the job runs")
}

// A failed teardown must exit non-zero with the stored reason: the account is
// left TERMINATING, and an operator who reads "done" would not go looking.
func TestFollowAccountDeletionReportsFailure(t *testing.T) {
	adminTestCredentials(t)
	defer cmd.SetAccountDeletePollInterval(time.Millisecond)()

	srv, _ := newScriptedAdminServer(t,
		`{"deletionId":"d-1","state":"FAILED","stages":[],"error":"2 resources left"}`)

	err := cmd.FollowAccountDeletion(t.Context(),
		cmd.AdminTarget(srv.URL, "us-west-1", writeServerCA(t, srv)), "000000000042")

	require.Error(t, err)
	assert.Contains(t, err.Error(), "2 resources left")
}

// Giving up on following is not the same as the teardown stopping, so the
// message has to name the account the caller must go back to.
func TestFollowAccountDeletionStopsWhenTheContextEnds(t *testing.T) {
	adminTestCredentials(t)
	defer cmd.SetAccountDeletePollInterval(50 * time.Millisecond)()

	srv, _ := newScriptedAdminServer(t, `{"deletionId":"d-1","state":"RUNNING","stages":[]}`)

	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()

	err := cmd.FollowAccountDeletion(ctx,
		cmd.AdminTarget(srv.URL, "us-west-1", writeServerCA(t, srv)), "000000000042")

	require.Error(t, err)
	assert.Contains(t, err.Error(), "000000000042")
}

// An unreadable job is not a finished one. Reporting it as complete would hide
// a teardown still holding an account's resources.
func TestFollowAccountDeletionFailsOnAnUnreadableJob(t *testing.T) {
	adminTestCredentials(t)
	defer cmd.SetAccountDeletePollInterval(time.Millisecond)()

	var got http.Request
	srv := newAdminTestServer(t, http.StatusNotFound,
		`{"error":{"code":"NoSuchEntity","message":"no job"},"requestId":"req-2"}`, &got)

	err := cmd.FollowAccountDeletion(t.Context(),
		cmd.AdminTarget(srv.URL, "us-west-1", writeServerCA(t, srv)), "000000000042")

	require.Error(t, err)
	assert.Contains(t, err.Error(), "NoSuchEntity")
}
