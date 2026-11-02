package cli_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mulgadc/spinifex/spinifex/operator/cli"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// deleteRecorder answers each admin call by method and records the order.
func deleteRecorder(t *testing.T, replies map[string]string) (*httptest.Server, func() []string) {
	t.Helper()
	var mu sync.Mutex
	var seen []string
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		method := strings.TrimPrefix(r.URL.Path, "/admin/")
		entry := method
		if strings.Contains(string(body), `"dryRun":true`) {
			entry += ":dryRun"
		}
		mu.Lock()
		seen = append(seen, entry)
		mu.Unlock()
		reply, ok := replies[entry]
		if !ok {
			reply = replies[method]
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(reply))
	}))
	t.Cleanup(srv.Close)
	return srv, func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), seen...)
	}
}

func runDeleteCmd(t *testing.T, srv *httptest.Server, flags map[string]string) error {
	t.Helper()
	adminTestCredentials(t)
	defer cli.SetAccountDeletePollInterval(time.Millisecond)()

	cmd := cli.AccountDeleteCmd
	all := map[string]string{
		"endpoint": srv.URL, "region": "us-west-1", "ca-bundle": writeServerCA(t, srv),
		"name": "tenant@example.com", "client-token": strings.Repeat("a", 32),
	}
	maps.Copy(all, flags)
	for k, v := range all {
		require.NoError(t, cmd.Flags().Set(k, v))
		t.Cleanup(func() { _ = cmd.Flags().Set(k, cmd.Flags().Lookup(k).DefValue) })
	}
	cmd.SetContext(t.Context())
	return cmd.RunE(cmd, []string{"000000000042"})
}

const (
	inventoryReply = `{"accountId":"000000000042","state":"DRY_RUN","dryRun":true,"inventory":{"AccountID":"000000000042","AccountName":"tenant@example.com"}}`
	startedReply   = `{"deletionId":"d-1","accountId":"000000000042","state":"RUNNING"}`
	doneReply      = `{"deletionId":"d-1","state":"COMPLETED","stages":[]}`
)

// Without --remote the command still goes through the admin API: it fetches the
// inventory, issues DeleteAccount, then follows DescribeAccountDeletion.
func TestAccountDeleteGoesThroughTheAdminAPI(t *testing.T) {
	srv, calls := deleteRecorder(t, map[string]string{
		"DeleteAccount:dryRun":    inventoryReply,
		"DeleteAccount":           startedReply,
		"DescribeAccountDeletion": doneReply,
	})

	var err error
	captureOutput(t, func() { err = runDeleteCmd(t, srv, map[string]string{"yes": "true"}) })
	require.NoError(t, err)
	t.Cleanup(func() { _ = cli.AccountDeleteCmd.Flags().Set("yes", "false") })

	assert.Equal(t, []string{"DeleteAccount:dryRun", "DeleteAccount", "DescribeAccountDeletion"}, calls())
}

// --remote is accepted so existing scripts keep working, and changes nothing.
func TestAccountDeleteAcceptsRemoteAsANoOp(t *testing.T) {
	srv, calls := deleteRecorder(t, map[string]string{
		"DeleteAccount:dryRun":    inventoryReply,
		"DeleteAccount":           startedReply,
		"DescribeAccountDeletion": doneReply,
	})

	var err error
	captureOutput(t, func() { err = runDeleteCmd(t, srv, map[string]string{"remote": "true", "yes": "true"}) })
	require.NoError(t, err)
	t.Cleanup(func() { _ = cli.AccountDeleteCmd.Flags().Set("yes", "false") })

	assert.Equal(t, []string{"DeleteAccount:dryRun", "DeleteAccount", "DescribeAccountDeletion"}, calls())
}

// A dry run reads the inventory through the API and issues no deletion.
func TestAccountDeleteDryRunNeverIssuesDeleteAccount(t *testing.T) {
	srv, calls := deleteRecorder(t, map[string]string{"DeleteAccount:dryRun": inventoryReply})

	var err error
	captureOutput(t, func() { err = runDeleteCmd(t, srv, map[string]string{"dry-run": "true"}) })
	require.NoError(t, err)

	assert.Equal(t, []string{"DeleteAccount:dryRun"}, calls())
}

// The CLI must never run a teardown itself: only the admin job may, because it
// holds the fenced deletion record. No non-test file in this package may build
// the engine or call Teardown.
func TestCLIHasNoPathToTeardown(t *testing.T) {
	files, err := filepath.Glob("*.go")
	require.NoError(t, err)
	require.NotEmpty(t, files)

	fset := token.NewFileSet()
	for _, file := range files {
		if strings.HasSuffix(file, "_test.go") {
			continue
		}
		parsed, err := parser.ParseFile(fset, file, nil, 0)
		require.NoError(t, err)
		ast.Inspect(parsed, func(n ast.Node) bool {
			if sel, ok := n.(*ast.SelectorExpr); ok {
				switch sel.Sel.Name {
				case "Teardown", "NewClusterEngine", "NewEngine":
					t.Errorf("%s calls %s: the CLI must delete through the admin API", fset.Position(sel.Pos()), sel.Sel.Name)
				}
			}
			return true
		})
	}
}
