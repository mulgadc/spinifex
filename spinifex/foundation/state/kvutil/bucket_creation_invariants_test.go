package kvutil_test

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// createFuncs are the JetStream calls that bring a KV bucket into existence.
// Each one takes a KeyValueConfig whose zero Replicas means one, so each one is
// a way to create a bucket on a single node without saying so.
var createFuncs = []string{"CreateKeyValue(", "CreateOrUpdateKeyValue("}

// exempt are the only packages allowed to call them: the chokepoint itself, the
// lease-bucket opener it shares its rule with, and the test helpers that build
// buckets for a single embedded server.
var exempt = []string{
	filepath.Join("spinifex", "foundation", "state", "kvutil"),
	filepath.Join("spinifex", "foundation", "state", "kvlease"),
	filepath.Join("internal", "testkit"),
}

// TestKVBuckets_AreOnlyCreatedThroughTheChokepoint keeps the replica-count rule
// enforceable by keeping the number of places that can break it at one.
//
// A KV bucket created with the default single replica puts a service every node
// depends on onto one node, so losing that node is a cluster-wide outage from a
// single-node event. The rule is that the cluster's node count decides the
// replica count, and a rule that lives in one function stays true only while
// that function is the only way through. This is what has failed before: there
// were five independent ways to create a bucket and the replica count was
// opt-in in all of them.
func TestKVBuckets_AreOnlyCreatedThroughTheChokepoint(t *testing.T) {
	root := repoRoot(t)

	type hit struct {
		file string
		line int
		call string
	}
	var hits []hit

	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", "vendor", "node_modules", "tests":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		rel, relErr := filepath.Rel(root, path)
		if relErr != nil {
			return relErr
		}
		for _, dir := range exempt {
			if strings.HasPrefix(rel, dir+string(filepath.Separator)) {
				return nil
			}
		}

		data, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		for i, line := range strings.Split(string(data), "\n") {
			for _, fn := range createFuncs {
				if strings.Contains(line, fn) {
					hits = append(hits, hit{file: rel, line: i + 1, call: strings.TrimSpace(line)})
				}
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}

	if len(hits) > 0 {
		var b strings.Builder
		for _, h := range hits {
			fmt.Fprintf(&b, "\n  %s:%d: %s", h.file, h.line, h.call)
		}
		t.Fatalf("a KV bucket is created outside kvutil, so its replica count is "+
			"whatever the literal happened to say — and an omitted Replicas is one, "+
			"on a cluster where every node depends on the bucket.\n"+
			"Use kvutil.GetOrCreateBucket, GetOrCreateBucketWithTTL or "+
			"GetOrCreateBucketWithOptions, which take the count from the cluster:%s", b.String())
	}
}

func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatalf("no go.mod above %s", dir)
		}
		dir = parent
	}
}
