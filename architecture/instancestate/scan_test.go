// Package instancestate freezes the production consumers of the daemon's raw
// instance-state persistence, so ADR-0007 S6 and S7 start from a known set
// that can only shrink.
package instancestate

import (
	"errors"
	"fmt"
	"go/ast"
	"go/build/constraint"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
)

const (
	daemonPath = "github.com/mulgadc/spinifex/spinifex/daemon"
	vmPath     = "github.com/mulgadc/spinifex/spinifex/runtime/compute/vm"
	daemonDir  = "spinifex/daemon"
	vmDir      = "spinifex/runtime/compute/vm"
)

// daemonSymbols are the daemon's instance-state bucket, key and local-file
// identifiers. The JetStreamManager type and constructor are not listed: they
// also serve cluster state, so only the instance-state methods below count.
var daemonSymbols = set(
	"InstanceStateBucket", "InstanceStateBucketVersion",
	"TerminatedInstanceBucket", "TerminatedInstanceBucketVersion",
	"InstanceRecordPrefix", "InstanceStatePrefix", "NodePresencePrefix",
	"StoppedInstancePrefix", "TerminatedInstancePrefix",
	"LocalState", "LocalStateSchemaVersion", "LocalStatePath", "ReadLocalState",
	"MarshalLocalState", "WriteLocalStateBytes", "LocalStateFileName",
	"LocalStateFileMode", "DefaultLocalStateDir",
)

// vmSymbols are the raw persisted record and its conversion.
var vmSymbols = set("InstanceRecord", "InstanceSpec", "InstanceStatus", "VMFromRecord")

// jsmMethods are JetStreamManager's instance-state methods. Matched by name, so
// a call through an interface that mirrors them counts as a consumer too.
var jsmMethods = set(
	"WriteInstanceRecord", "LoadInstanceRecord", "UpdateInstanceRecord",
	"DeleteInstanceRecord", "ListInstanceRecords",
	"WriteTerminatedInstanceRecord", "LoadTerminatedInstanceRecord",
	"UpdateTerminatedInstanceRecord", "DeleteTerminatedInstanceRecord",
	"ListTerminatedInstanceRecords",
	"WriteNodeMarker", "WriteNodeMarkerBestEffort", "WriteRunningSet",
	"WriteStoppedInstance", "LoadStoppedInstance", "DeleteStoppedInstance",
	"ClaimStoppedInstance", "ClaimRecoverableInstance", "ReleaseRecoveredInstance",
	"AbandonRecovery", "UpdateStoppedInstance", "ListStoppedInstances",
	"WriteTerminatedInstance", "UpdateTerminatedInstance", "ListTerminatedInstances",
	"DeleteTerminatedInstance", "LoadTerminatedInstance",
	"InitKVBucket", "InitTerminatedInstanceBucket",
)

// ambiguousJSMMethods share a name with unrelated methods, so they count only
// when called on a field or variable named like a JetStreamManager handle.
var ambiguousJSMMethods = set("LoadState", "DeleteState")

// literals duplicate a bucket or file name instead of naming the owner's symbol.
var literals = set(`"spinifex-instance-state"`, `"spinifex-terminated-instances"`, `"instance-state.json"`)

// ownerFiles define the symbols above. They are the implementation being
// migrated, not consumers of it, and are listed so a deleted one is noticed.
var ownerFiles = []string{
	"spinifex/daemon/instance_membership.go",
	"spinifex/daemon/instance_records.go",
	"spinifex/daemon/instance_records_migrate.go",
	"spinifex/daemon/instance_running_set.go",
	"spinifex/daemon/jetstream.go",
	"spinifex/daemon/local_state.go",
	"spinifex/runtime/compute/vm/record.go",
}

func set(names ...string) map[string]bool {
	m := make(map[string]bool, len(names))
	for _, n := range names {
		m[n] = true
	}
	return m
}

// use is one consumer of one symbol, keyed by file so a new file is a new use.
type use struct {
	File, Symbol string
}

func (u use) String() string { return u.File + " uses " + u.Symbol }

// scan maps each use to the first file:line it appears at.
type scan map[use]string

func moduleRoot() (string, error) {
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		return "", errors.New("cannot locate test source")
	}
	return filepath.Abs(filepath.Join(filepath.Dir(file), "..", ".."))
}

var excludedTopDirs = map[string]bool{"tests": true, "docs": true, "architecture": true}

// scanModule parses every non-test production Go file under root.
func scanModule(root string) (scan, int, error) {
	out := scan{}
	files := 0
	fset := token.NewFileSet()
	owners := set(ownerFiles...)
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if d.IsDir() {
			if rel == "." {
				return nil
			}
			name := d.Name()
			if strings.HasPrefix(name, ".") || strings.HasPrefix(name, "_") ||
				name == "testdata" || name == "vendor" || name == "node_modules" || excludedTopDirs[rel] {
				return filepath.SkipDir
			}
			if _, err := os.Stat(filepath.Join(path, "go.mod")); err == nil {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		src, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		f, err := parser.ParseFile(fset, path, src, parser.ParseComments|parser.SkipObjectResolution)
		if err != nil {
			return err
		}
		if ignoredByBuildTag(f.Comments, f.Package) {
			return nil
		}
		files++
		if owners[rel] {
			return nil
		}
		scanFile(fset, rel, f, out)
		return nil
	})
	return out, files, err
}

// scanFile records every use in f, which lives at module-relative path rel.
func scanFile(fset *token.FileSet, rel string, f *ast.File, out scan) {
	dir := filepath.ToSlash(filepath.Dir(rel))
	daemonName, vmName := importName(f, daemonPath, "daemon"), importName(f, vmPath, "vm")
	inDaemon, inVM := dir == daemonDir, dir == vmDir
	seesVM := inDaemon || inVM || daemonName != "" || vmName != ""

	record := func(symbol string, pos token.Pos) {
		u := use{File: rel, Symbol: symbol}
		if _, ok := out[u]; !ok {
			out[u] = fmt.Sprintf("%s:%d", rel, fset.Position(pos).Line)
		}
	}

	var visit func(n ast.Node) bool
	visit = func(n ast.Node) bool {
		switch n := n.(type) {
		case *ast.ImportSpec:
			return false
		case *ast.BasicLit:
			if n.Kind == token.STRING && literals[n.Value] {
				record("literal "+n.Value, n.Pos())
			}
		case *ast.CallExpr:
			if sel, ok := n.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "Record" && len(n.Args) == 0 && seesVM {
				record("VM.Record()", sel.Sel.Pos())
			}
		case *ast.SelectorExpr:
			if x, ok := n.X.(*ast.Ident); ok {
				switch {
				case daemonName != "" && x.Name == daemonName && daemonSymbols[n.Sel.Name]:
					record("daemon."+n.Sel.Name, n.Sel.Pos())
				case vmName != "" && x.Name == vmName && vmSymbols[n.Sel.Name]:
					record("vm."+n.Sel.Name, n.Sel.Pos())
				}
			}
			if jsmMethods[n.Sel.Name] || (ambiguousJSMMethods[n.Sel.Name] && jsmReceivers[receiverName(n.X)]) {
				record("JetStreamManager."+n.Sel.Name, n.Sel.Pos())
			}
			// The selected name is a field or method, never a bare package symbol.
			ast.Inspect(n.X, visit)
			return false
		case *ast.FuncDecl:
			// A method declared with one of these names is an implementation,
			// not a use; its body is still scanned.
			if n.Recv != nil {
				ast.Inspect(n.Recv, visit)
			}
			if n.Body != nil {
				ast.Inspect(n.Body, visit)
			}
			if n.Type != nil {
				ast.Inspect(n.Type, visit)
			}
			return false
		case *ast.Field:
			// Interface method and struct field names are declarations.
			if n.Type != nil {
				ast.Inspect(n.Type, visit)
			}
			if n.Tag != nil {
				ast.Inspect(n.Tag, visit)
			}
			return false
		case *ast.Ident:
			if inDaemon && daemonSymbols[n.Name] {
				record("daemon."+n.Name, n.Pos())
			}
			if inVM && vmSymbols[n.Name] {
				record("vm."+n.Name, n.Pos())
			}
		}
		return true
	}
	ast.Inspect(f, visit)
}

var jsmReceivers = set("jsManager", "jsm", "js")

// receiverName is the last name in x: "js" for both js and a.js.
func receiverName(x ast.Expr) string {
	switch x := x.(type) {
	case *ast.Ident:
		return x.Name
	case *ast.SelectorExpr:
		return x.Sel.Name
	}
	return ""
}

// importName returns the name f refers to path by, or "" if f does not import it.
func importName(f *ast.File, path, def string) string {
	for _, imp := range f.Imports {
		p, err := strconv.Unquote(imp.Path.Value)
		if err != nil || p != path {
			continue
		}
		if imp.Name != nil {
			if imp.Name.Name == "_" || imp.Name.Name == "." {
				return ""
			}
			return imp.Name.Name
		}
		return def
	}
	return ""
}

// ignoredByBuildTag reports a `//go:build ignore` file, which is a standalone
// program rather than part of its directory's package.
func ignoredByBuildTag(groups []*ast.CommentGroup, pkgPos token.Pos) bool {
	for _, cg := range groups {
		if cg.Pos() >= pkgPos {
			break
		}
		for _, c := range cg.List {
			if !constraint.IsGoBuild(c.Text) {
				continue
			}
			expr, err := constraint.Parse(c.Text)
			if err == nil && expr.String() == "ignore" {
				return true
			}
		}
	}
	return false
}

func (s scan) sorted() []use {
	out := make([]use, 0, len(s))
	for u := range s {
		out = append(out, u)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].File != out[j].File {
			return out[i].File < out[j].File
		}
		return out[i].Symbol < out[j].Symbol
	})
	return out
}
