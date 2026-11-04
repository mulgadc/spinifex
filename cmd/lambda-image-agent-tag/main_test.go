package main

import (
	"bytes"
	"compress/gzip"
	"debug/buildinfo"
	"fmt"
	"os"
	"path/filepath"
	"runtime/debug"
	"testing"

	"github.com/stretchr/testify/require"
)

type fileEntry struct {
	name  string
	mode  uint32
	nlink uint32
	data  string
}

func dir(name string) fileEntry { return fileEntry{name: name, mode: modeDir | 0o755, nlink: 2} }
func file(name, data string) fileEntry {
	return fileEntry{name: name, mode: modeRegular | 0o755, nlink: 1, data: data}
}
func symlink(name, target string) fileEntry {
	return fileEntry{name: name, mode: 0o120777, nlink: 1, data: target}
}

// newc writes entries and a trailer in the format the kernel unpacks.
func newc(entries ...fileEntry) []byte {
	var b bytes.Buffer
	for i, e := range append(entries, fileEntry{name: trailerName, nlink: 1}) {
		fmt.Fprintf(&b, "070701%08X%08X%08X%08X%08X%08X%08X%08X%08X%08X%08X%08X%08X",
			i+1, e.mode, 0, 0, e.nlink, 0, len(e.data), 0, 0, 0, 0, len(e.name)+1, 0)
		b.WriteString(e.name)
		b.WriteByte(0)
		pad(&b)
		b.WriteString(e.data)
		pad(&b)
	}
	return b.Bytes()
}

func pad(b *bytes.Buffer) {
	for b.Len()%4 != 0 {
		b.WriteByte(0)
	}
}

func gz(parts ...[]byte) []byte {
	var b bytes.Buffer
	for _, p := range parts {
		w := gzip.NewWriter(&b)
		_, _ = w.Write(p)
		_ = w.Close()
	}
	return b.Bytes()
}

func withAgent(agent fileEntry) []byte {
	return newc(dir("."), dir("usr"), dir("usr/local"), dir("usr/local/bin"), agent, file("init", "#!/bin/sh"))
}

func TestArchivedAgent_ReturnsTheOneArchivedAgent(t *testing.T) {
	got, err := archivedAgent(bytes.NewReader(gz(withAgent(file(agentPath, "agent-bytes")))))
	require.NoError(t, err)
	require.Equal(t, "agent-bytes", string(got))

	padded := append(withAgent(file(agentPath, "agent-bytes")), make([]byte, 1024)...)
	got, err = archivedAgent(bytes.NewReader(gz(padded)))
	require.NoError(t, err, "zero padding after the trailer")
	require.Equal(t, "agent-bytes", string(got))
}

func TestArchivedAgent_Refuses(t *testing.T) {
	parents := []fileEntry{dir("."), dir("usr"), dir("usr/local"), dir("usr/local/bin")}
	agent := file(agentPath, "untagged")
	small := newc(append(parents, agent)...)
	second := newc(append(parents[1:], file(agentPath, "tagged"))...)
	// The second archive starts inside what would be the first one's final block.
	sameBlock := append(append([]byte{}, small...), second...)
	for len(sameBlock)%512 != 0 {
		sameBlock = append(sameBlock, 0)
	}

	for name, archive := range map[string][]byte{
		"aliased duplicate":            gz(newc(append(parents, agent, file("././"+agentPath, "tagged"))...)),
		"leading slash alias":          gz(newc(append(parents, agent, file("/"+agentPath, "tagged"))...)),
		"dot-dot component":            gz(newc(append(parents, agent, file("usr/local/../local/bin/lambda-agent", "x"))...)),
		"empty component":              gz(newc(append(parents, agent, file("usr//local/bin/lambda-agent", "x"))...)),
		"exact duplicate":              gz(newc(append(parents, agent, file(agentPath, "tagged"))...)),
		"duplicate directory":          gz(newc(dir("usr"), dir("usr"), dir("usr/local"), dir("usr/local/bin"), agent)),
		"missing directory entry":      gz(newc(dir("usr"), dir("usr/local/bin"), agent)),
		"directory after the agent":    gz(newc(dir("usr"), dir("usr/local"), agent, dir("usr/local/bin"))),
		"parent is a symlink":          gz(newc(dir("usr"), dir("usr/local"), symlink("usr/local/bin", "elsewhere"), agent)),
		"agent is a symlink":           gz(newc(append(parents, symlink(agentPath, "/bin/sh"))...)),
		"hard-linked agent":            gz(newc(append(parents, fileEntry{name: agentPath, mode: modeRegular | 0o755, nlink: 2, data: "x"})...)),
		"no agent":                     gz(newc(parents...)),
		"second archive appended":      gz(small, second),
		"second archive in same block": gz(sameBlock),
		"non-zero byte after trailer":  gz(append(append([]byte{}, small...), 0, 0, 1)),
		"truncated archive":            gz(small[:len(small)-20]),
		"not newc":                     gz([]byte("not cpio at all, but long enough to fill one newc header: " + string(make([]byte, 120)))),
		"not gzip":                     small,
	} {
		t.Run(name, func(t *testing.T) {
			_, err := archivedAgent(bytes.NewReader(archive))
			require.Error(t, err)
		})
	}
}

func TestRun_RefusesASymlinkedImagePath(t *testing.T) {
	d := t.TempDir()
	img := filepath.Join(d, "initramfs.cpio.gz")
	require.NoError(t, os.WriteFile(img, gz(withAgent(file(agentPath, "x"))), 0o644))
	require.NoError(t, os.Symlink(img, filepath.Join(d, "link")))
	require.ErrorContains(t, run(filepath.Join(d, "link"), false), "not a regular file")
}

func TestHasTag_MatchesWholeTagsOnly(t *testing.T) {
	info := func(tags string) *buildinfo.BuildInfo {
		return &buildinfo.BuildInfo{Settings: []debug.BuildSetting{{Key: "-buildmode", Value: "exe"}, {Key: "-tags", Value: tags}}}
	}
	require.True(t, hasTag(info("lambdatestlink,fips140v1.0"), "lambdatestlink"))
	require.False(t, hasTag(info("fips140v1.0"), "lambdatestlink"))
	require.False(t, hasTag(info("lambdatestlinkx,xlambdatestlink"), "lambdatestlink"))
	require.False(t, hasTag(&buildinfo.BuildInfo{}, "lambdatestlink"))
}

// The test binary carries real build information, so checkTag reads it and
// refuses the opposite answer; bytes with no build information are refused.
func TestCheckTag_ReadsRealBuildInformation(t *testing.T) {
	exe, err := os.Executable()
	require.NoError(t, err)
	raw, err := os.ReadFile(exe)
	require.NoError(t, err)
	info, err := buildinfo.Read(bytes.NewReader(raw))
	require.NoError(t, err)
	tagged := hasTag(info, "lambdatestlink")
	require.NoError(t, checkTag(raw, tagged))
	require.Error(t, checkTag(raw, !tagged))
	require.Error(t, checkTag([]byte("not a Go binary"), false))
}

func TestDecide_RefusesMissingBuildSettings(t *testing.T) {
	require.ErrorContains(t, decide(&buildinfo.BuildInfo{}, false), "no build settings")
	require.ErrorContains(t, decide(&buildinfo.BuildInfo{}, true), "no build settings")
	untagged := &buildinfo.BuildInfo{Settings: []debug.BuildSetting{{Key: "-buildmode", Value: "exe"}}}
	require.NoError(t, decide(untagged, false))
	require.Error(t, decide(untagged, true))
}
