package segscanoracle

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDecodeReport_Valid(t *testing.T) {
	data := []byte(`{"totals":{"liveLogical":10,"livePhysical":20,"deadTombstonedReclaimable":30,"deadOrphanUnreclaimable":40}}`)

	rep, err := decodeReport(data)
	require.NoError(t, err)
	assert.Equal(t, int64(10), rep.Totals.LiveLogical)
	assert.Equal(t, int64(20), rep.Totals.LivePhysical)
	assert.Equal(t, int64(30), rep.Totals.DeadTombstoned)
	assert.Equal(t, int64(40), rep.Totals.DeadOrphan)
}

func TestDecodeReport_Malformed(t *testing.T) {
	_, err := decodeReport([]byte(`{not valid json`))
	require.Error(t, err)
}

func TestResolveSegscanDir(t *testing.T) {
	t.Run("env override with go.mod", func(t *testing.T) {
		dir := t.TempDir()
		require.NoError(t, os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module fake\n"), 0644))

		got, err := resolveSegscanDir(dir, "/does/not/matter")
		require.NoError(t, err)
		assert.Equal(t, dir, got)
	})

	t.Run("env override without go.mod", func(t *testing.T) {
		dir := t.TempDir()

		_, err := resolveSegscanDir(dir, "/does/not/matter")
		require.Error(t, err)
	})

	t.Run("monorepo layout candidate", func(t *testing.T) {
		base := t.TempDir()
		root := filepath.Join(base, "mulga", "spinifex")
		segscanDir := filepath.Join(base, "mulga", "scripts", "segscan")
		require.NoError(t, os.MkdirAll(segscanDir, 0755))
		require.NoError(t, os.WriteFile(filepath.Join(segscanDir, "go.mod"), []byte("module fake\n"), 0644))

		got, err := resolveSegscanDir("", root)
		require.NoError(t, err)
		assert.Equal(t, segscanDir, got)
	})

	t.Run("CI sparse-checkout sibling layout candidate", func(t *testing.T) {
		base := t.TempDir()
		root := filepath.Join(base, "workspace", "spinifex")
		segscanDir := filepath.Join(base, "workspace", "mulga", "scripts", "segscan")
		require.NoError(t, os.MkdirAll(segscanDir, 0755))
		require.NoError(t, os.WriteFile(filepath.Join(segscanDir, "go.mod"), []byte("module fake\n"), 0644))

		got, err := resolveSegscanDir("", root)
		require.NoError(t, err)
		assert.Equal(t, segscanDir, got)
	})

	t.Run("not found", func(t *testing.T) {
		base := t.TempDir()
		root := filepath.Join(base, "isolated", "spinifex")
		require.NoError(t, os.MkdirAll(root, 0755))

		_, err := resolveSegscanDir("", root)
		require.Error(t, err)
	})
}

func TestHasGoMod(t *testing.T) {
	dir := t.TempDir()
	assert.False(t, hasGoMod(dir), "empty dir has no go.mod")

	require.NoError(t, os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module fake\n"), 0644))
	assert.True(t, hasGoMod(dir))

	// A directory literally named "go.mod" must not count as the file.
	dir2 := t.TempDir()
	require.NoError(t, os.Mkdir(filepath.Join(dir2, "go.mod"), 0755))
	assert.False(t, hasGoMod(dir2))
}

func TestCopyNodeDir(t *testing.T) {
	src := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(src, "db"), 0755))
	require.NoError(t, os.WriteFile(filepath.Join(src, "state.json"), []byte(`{"segNum":1}`), 0644))
	require.NoError(t, os.WriteFile(filepath.Join(src, "db", "MANIFEST"), []byte("manifest-bytes"), 0644))
	require.NoError(t, os.WriteFile(filepath.Join(src, "0000000000000001.seg"), []byte("segment-bytes"), 0644))

	srcBefore := snapshotDir(t, src)

	dst := CopyNodeDir(t, src)

	assert.Equal(t, srcBefore, snapshotDir(t, src), "source dir must be untouched by the copy")

	dstState, err := os.ReadFile(filepath.Join(dst, "state.json"))
	require.NoError(t, err)
	assert.Equal(t, `{"segNum":1}`, string(dstState))

	dstManifest, err := os.ReadFile(filepath.Join(dst, "db", "MANIFEST"))
	require.NoError(t, err)
	assert.Equal(t, "manifest-bytes", string(dstManifest))

	dstSeg, err := os.ReadFile(filepath.Join(dst, "0000000000000001.seg"))
	require.NoError(t, err)
	assert.Equal(t, "segment-bytes", string(dstSeg))
}

// TestRun_WithFakeSegscan exercises build+exec+decode end to end against a
// throwaway stand-in module rather than the real scripts/segscan (which is
// only available in the mulga umbrella repo, not in this module) — it just
// needs to be a buildable Go program that prints a --json-shaped payload, to
// prove Run's plumbing (compiling SEGSCAN_DIR, execing it with --dir/--json,
// decoding the result) actually works.
func TestRun_WithFakeSegscan(t *testing.T) {
	fakeSrc := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(fakeSrc, "go.mod"), []byte("module fakesegscan\n\ngo 1.21\n"), 0644))
	mainGo := `package main

import "fmt"

func main() {
	fmt.Print(` + "`" + `{"totals":{"liveLogical":1,"livePhysical":2,"deadTombstonedReclaimable":3,"deadOrphanUnreclaimable":4}}` + "`" + `)
}
`
	require.NoError(t, os.WriteFile(filepath.Join(fakeSrc, "main.go"), []byte(mainGo), 0644))

	t.Setenv(segscanEnvVar, fakeSrc)

	rep := Run(t, t.TempDir())
	assert.Equal(t, int64(1), rep.Totals.LiveLogical)
	assert.Equal(t, int64(2), rep.Totals.LivePhysical)
	assert.Equal(t, int64(3), rep.Totals.DeadTombstoned)
	assert.Equal(t, int64(4), rep.Totals.DeadOrphan)
}

// snapshotDir maps every regular file under dir (relative path) to its
// content, for asserting a directory tree is byte-identical before/after an
// operation that must not have touched it.
func snapshotDir(t *testing.T, dir string) map[string]string {
	t.Helper()
	snap := make(map[string]string)
	err := filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		snap[rel] = string(data)
		return nil
	})
	require.NoError(t, err)
	return snap
}
