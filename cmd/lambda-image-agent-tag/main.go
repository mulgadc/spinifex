// Command lambda-image-agent-tag checks whether the lambda-agent archived in a
// built Lambda guest initramfs carries the lambdatestlink build tag. Build
// information is recorded by the build; it is not proof of how the agent behaves.
package main

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"debug/buildinfo"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strconv"
	"strings"
)

const (
	agentPath    = "usr/local/bin/lambda-agent"
	maxArchive   = 1 << 30
	maxAgent     = 256 << 20
	maxName      = 4096
	newcHeader   = 110
	trailerName  = "TRAILER!!!"
	modeTypeMask = 0o170000
	modeDir      = 0o040000
	modeRegular  = 0o100000
)

var agentParents = []string{"usr", "usr/local", "usr/local/bin"}

func main() {
	if len(os.Args) != 3 || (os.Args[2] != "present" && os.Args[2] != "absent") {
		fmt.Fprintln(os.Stderr, "usage: lambda-image-agent-tag <initramfs.cpio.gz> present|absent")
		os.Exit(2)
	}
	if err := run(os.Args[1], os.Args[2] == "present"); err != nil {
		fmt.Fprintf(os.Stderr, "%s: %v\n", os.Args[1], err)
		os.Exit(1)
	}
}

func run(path string, want bool) error {
	fi, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !fi.Mode().IsRegular() {
		return errors.New("not a regular file")
	}
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	agent, err := archivedAgent(f)
	if err != nil {
		return err
	}
	return checkTag(agent, want)
}

// archivedAgent returns the single agent entry after checking listed names,
// entry types, the agent's link count and zero-only padding after the trailer.
// It is not a full kernel-equivalent parser; 070702 CRCs, for one, are unchecked.
func archivedAgent(r io.Reader) ([]byte, error) {
	zr, err := gzip.NewReader(bufio.NewReader(r))
	if err != nil {
		return nil, fmt.Errorf("not a readable gzip archive: %w", err)
	}
	raw, err := io.ReadAll(io.LimitReader(zr, maxArchive+1))
	if err != nil {
		return nil, fmt.Errorf("not a readable gzip archive: %w", err)
	}
	if len(raw) > maxArchive {
		return nil, fmt.Errorf("archive larger than %d bytes", maxArchive)
	}

	seen := map[string]bool{}
	var agent []byte
	off := 0
	for {
		e, next, err := parseEntry(raw, off)
		if err != nil {
			return nil, fmt.Errorf("entry at offset %d: %w", off, err)
		}
		off = next
		if e.name == trailerName {
			break
		}
		if !canonical(e.name) {
			return nil, fmt.Errorf("entry name %q is not canonical", e.name)
		}
		if seen[e.name] {
			return nil, fmt.Errorf("entry %q appears more than once", e.name)
		}
		seen[e.name] = true
		for _, p := range agentParents {
			if e.name == p && e.mode&modeTypeMask != modeDir {
				return nil, fmt.Errorf("%s is not a directory", p)
			}
		}
		if e.name == agentPath {
			for _, p := range agentParents {
				if !seen[p] {
					return nil, fmt.Errorf("%s has no %s directory entry before it", agentPath, p)
				}
			}
			if e.mode&modeTypeMask != modeRegular {
				return nil, fmt.Errorf("%s is not a regular file", agentPath)
			}
			if e.nlink != 1 {
				return nil, fmt.Errorf("%s has %d links, want 1", agentPath, e.nlink)
			}
			if len(e.data) > maxAgent {
				return nil, fmt.Errorf("%s is larger than %d bytes", agentPath, maxAgent)
			}
			agent = e.data
		}
	}
	if i := bytes.IndexFunc(raw[off:], func(r rune) bool { return r != 0 }); i >= 0 {
		return nil, fmt.Errorf("data after the archive trailer at offset %d", off+i)
	}
	if agent == nil {
		return nil, fmt.Errorf("no %s entry", agentPath)
	}
	return agent, nil
}

type entry struct {
	name  string
	mode  uint64
	nlink uint64
	data  []byte
}

func parseEntry(raw []byte, off int) (entry, int, error) {
	if len(raw)-off < newcHeader {
		return entry{}, 0, errors.New("truncated header")
	}
	h := raw[off : off+newcHeader]
	if m := string(h[:6]); m != "070701" && m != "070702" {
		return entry{}, 0, fmt.Errorf("magic %q is not newc", m)
	}
	field := func(i int) (uint64, error) {
		return strconv.ParseUint(string(h[6+8*i:14+8*i]), 16, 32)
	}
	mode, err1 := field(1)
	nlink, err2 := field(4)
	size, err3 := field(6)
	nameSize, err4 := field(11)
	if err := errors.Join(err1, err2, err3, err4); err != nil {
		return entry{}, 0, fmt.Errorf("bad header field: %w", err)
	}
	if nameSize < 2 || nameSize > maxName {
		return entry{}, 0, fmt.Errorf("name size %d out of range", nameSize)
	}
	nameEnd := off + newcHeader + int(nameSize)
	dataOff := align4(nameEnd)
	dataEnd := dataOff + int(size)
	if dataEnd > len(raw) || align4(dataEnd) > len(raw) {
		return entry{}, 0, errors.New("truncated entry")
	}
	name := raw[off+newcHeader : nameEnd]
	if name[len(name)-1] != 0 || bytes.IndexByte(name[:len(name)-1], 0) >= 0 {
		return entry{}, 0, errors.New("name is not one NUL-terminated string")
	}
	return entry{name: string(name[:len(name)-1]), mode: mode, nlink: nlink, data: raw[dataOff:dataEnd]}, align4(dataEnd), nil
}

func align4(n int) int { return (n + 3) &^ 3 }

// canonical accepts "." and relative names with no empty, "." or ".." component.
func canonical(name string) bool {
	if name == "." {
		return true
	}
	if name == "" || strings.HasPrefix(name, "/") {
		return false
	}
	for c := range strings.SplitSeq(name, "/") {
		if c == "" || c == "." || c == ".." {
			return false
		}
	}
	return true
}

func checkTag(agent []byte, want bool) error {
	info, err := buildinfo.Read(bytes.NewReader(agent))
	if err != nil {
		return fmt.Errorf("no Go build information in %s: %w", agentPath, err)
	}
	return decide(info, want)
}

// decide refuses build information with no recorded settings, since their
// absence does not show the tag was absent.
func decide(info *buildinfo.BuildInfo, want bool) error {
	if len(info.Settings) == 0 {
		return fmt.Errorf("%s records no build settings", agentPath)
	}
	if hasTag(info, "lambdatestlink") != want {
		return fmt.Errorf("%s: lambdatestlink build tag present=%v, want %v", agentPath, !want, want)
	}
	return nil
}

func hasTag(info *buildinfo.BuildInfo, tag string) bool {
	for _, s := range info.Settings {
		if s.Key != "-tags" {
			continue
		}
		if slices.Contains(strings.Split(s.Value, ","), tag) {
			return true
		}
	}
	return false
}
