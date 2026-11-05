//go:build lambdatestlink

package testlink

import (
	"bytes"
	"context"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/mulgadc/spinifex/spinifex/domains/lambda/execution"
	"github.com/stretchr/testify/require"
)

// liveStub makes the package's QEMU stub stay running, so attempt.Run starts a
// process that is still alive when its identity is read; the exiting stub
// returns afterwards.
func liveStub(t *testing.T) {
	t.Helper()
	path, err := exec.LookPath("qemu-system-x86_64")
	require.NoError(t, err)
	orig, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Contains(t, string(orig), "fake qemu-system-x86_64", "PATH does not resolve to the package's stub")
	require.NoError(t, os.WriteFile(path, []byte("#!/bin/sh\nexec sleep 60\n"), 0o755))
	t.Cleanup(func() { _ = os.WriteFile(path, orig, 0o755) })
}

// failLaunched is the owner with its launch-identity write failing; it keeps
// the identity it was asked to record.
type failLaunched struct {
	Grants
	got []execution.Launch
}

func (f *failLaunched) Launched(_ context.Context, _ execution.Grant, l execution.Launch) error {
	f.got = append(f.got, l)
	return errors.New("injected launch-identity write failure")
}

// A process started before its launch identity is recorded replies unknown,
// never refused. Neither the files the attempt left behind, the reply nor the
// log carries the credential or the code URL.
func TestDaemon_StartThenFailedLaunchedWriteIsUnknown(t *testing.T) {
	liveStub(t)
	var logs bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	code := []byte("zip bytes")
	srv := codeServer(t, code)
	g := grant(t, code)
	d := g.daemon(&Daemon{IPs: &fakeIPs{}, Taps: &fakeTaps{}, HTTP: srv.Client(), WorkDir: t.TempDir()})
	owner := &failLaunched{Grants: d.Grants}
	d.Grants = owner

	rep := d.Handle(context.Background(), g.request(srv.URL, code), testCred)
	// An identity is read only from a live process's pidfd, so one was started.
	require.Len(t, owner.got, 1, "no process was started, so this run proves nothing about after-start replies")
	require.NotZero(t, owner.got[0].PID)
	require.NotZero(t, owner.got[0].Inode)
	require.Equal(t, OutcomeUnknown, rep.Outcome, rep.Error)
	require.Equal(t, ErrLaunchNotRecorded.Error(), rep.Error)

	secrets := []string{testCred.AccessKeyID, testCred.SecretAccessKey, testCred.SessionToken, srv.URL}
	files := 0
	require.NoError(t, filepath.WalkDir(d.WorkDir, func(p string, e fs.DirEntry, err error) error {
		if err != nil || e.IsDir() {
			return err
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		files++
		for _, s := range secrets {
			require.NotContains(t, string(b), s, "%s holds a secret", p)
		}
		return nil
	}))
	require.NotZero(t, files, "the attempt left no files to check")
	for _, s := range secrets {
		require.NotContains(t, rep.Error, s)
		require.NotContains(t, logs.String(), s)
	}
}

// Each direct refused() call in run sits before its one attempt.Run, and
// session and sessionReply make none. Indirect constructors are not covered;
// the injected post-start failure above is the behavioural check.
func TestDaemon_RefusedRepliesPrecedeTheVMStart(t *testing.T) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "daemon.go", nil, 0)
	require.NoError(t, err)
	funcs := map[string]*ast.FuncDecl{}
	for _, decl := range f.Decls {
		if fn, ok := decl.(*ast.FuncDecl); ok {
			funcs[fn.Name.Name] = fn
		}
	}
	calls := func(fn *ast.FuncDecl, match func(*ast.CallExpr) bool) []token.Pos {
		var at []token.Pos
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			if c, ok := n.(*ast.CallExpr); ok && match(c) {
				at = append(at, c.Pos())
			}
			return true
		})
		return at
	}
	isRefused := func(c *ast.CallExpr) bool { id, ok := c.Fun.(*ast.Ident); return ok && id.Name == "refused" }
	isRun := func(c *ast.CallExpr) bool {
		sel, ok := c.Fun.(*ast.SelectorExpr)
		pkg, isPkg := func() (*ast.Ident, bool) {
			if !ok {
				return nil, false
			}
			id, ok := sel.X.(*ast.Ident)
			return id, ok
		}()
		return isPkg && pkg.Name == "attempt" && sel.Sel.Name == "Run"
	}

	run := funcs["run"]
	require.NotNil(t, run)
	start := calls(run, isRun)
	require.Len(t, start, 1, "run must start the VM exactly once")
	refusals := calls(run, isRefused)
	require.NotEmpty(t, refusals)
	for _, p := range refusals {
		require.Less(t, p, start[0], "refused at %s follows the VM start", fset.Position(p))
	}
	for _, name := range []string{"session", "sessionReply"} {
		require.NotNil(t, funcs[name], name)
		require.Empty(t, calls(funcs[name], isRefused), "%s builds a refused reply after the VM start", name)
	}
}
