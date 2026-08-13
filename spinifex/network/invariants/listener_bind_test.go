package invariants

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/mulgadc/spinifex/spinifex/network/listenerinventory"
)

// TestListenerBindSitesMatchInventory is the static half of the listener
// invariant: docs/security/network-connections/README.md "## 1. Inbound
// Listeners" classifies every port's intended reach (External/Cluster/
// Encap/Guest/Localhost). This test scans the install scripts and config
// templates for the two bind patterns known to put a listener on the wrong
// plane — a bare OVSDB `ptcp:PORT` remote, which binds the wildcard address
// by default, and a literal wildcard `0.0.0.0:PORT` / `:PORT` bind — and
// fails if either lands on a Cluster/Encap port the inventory has not
// explicitly excused, or on a port the inventory does not document at all.
//
// Unlike every other test in this package, it reads text, not Go: the
// defect it targets (spinifex #765, the OVN NB/SB wildcard bind) left no
// trace in an import graph or an AST, only in a shell script's set-connection
// call. It was found by hand on a live node; this is what would have caught
// it at review time.
func TestListenerBindSitesMatchInventory(t *testing.T) {
	root := repoRoot(t)
	docPath := listenerinventory.DocPath(root)
	table, err := listenerinventory.ParseFile(docPath)
	if err != nil {
		t.Fatalf("parse %s: %v", relTo(docPath, root), err)
	}

	var bad []string
	for _, f := range listenerBindScanFiles(t, root) {
		sites, serr := scanBindSites(f)
		if serr != nil {
			t.Fatalf("scan %s: %v", relTo(f, root), serr)
		}
		for _, s := range sites {
			if msg := checkBindSite(table, s, root); msg != "" {
				bad = append(bad, msg)
			}
		}
	}

	if len(bad) == 0 {
		return
	}
	sort.Strings(bad)

	var b strings.Builder
	b.WriteString(`docs/security/network-connections/README.md "## 1. Inbound ` +
		`Listeners" is the fixture for every bind site below; add or correct a ` +
		"row there rather than widening this test.\n")
	limit := 10
	for i, m := range bad {
		if i >= limit {
			b.WriteString("  …\n")
			break
		}
		b.WriteString("  ")
		b.WriteString(m)
		b.WriteString("\n")
	}
	if len(bad) > limit {
		b.WriteString("  ")
		b.WriteString(itoa(len(bad) - limit))
		b.WriteString(" further violations suppressed.\n")
	}
	t.Fatalf("%s", b.String())
}

// listenerBindScanFiles returns the install scripts and config templates
// known to write listener bind addresses. Each is required to exist so a
// rename silently drops coverage instead of narrowing it.
func listenerBindScanFiles(t *testing.T, root string) []string {
	t.Helper()
	files := []string{
		filepath.Join(root, "scripts", "setup-ovn.sh"),
		filepath.Join(root, "scripts", "install-node.sh"),
		filepath.Join(root, "scripts", "setup.sh"),
	}
	tomls, err := filepath.Glob(filepath.Join(root, "cmd", "spinifex", "cmd", "templates", "*.toml"))
	if err != nil {
		t.Fatalf("glob templates: %v", err)
	}
	sort.Strings(tomls)
	files = append(files, tomls...)
	for _, f := range files {
		if _, statErr := os.Stat(f); statErr != nil {
			t.Fatalf("listener bind scan target missing: %s: %v", f, statErr)
		}
	}
	return files
}

// bindSite is one candidate listener bind found by scanning a script or
// template: an OVSDB ptcp remote, or a literal wildcard address:port pair.
type bindSite struct {
	file     string
	line     int
	port     int
	wildcard bool
	barePtcp bool
	raw      string
}

var (
	// ptcpRe matches an OVSDB ptcp remote: `ptcp:PORT` alone, or
	// `ptcp:PORT:ADDR` with an explicit bind address.
	ptcpRe = regexp.MustCompile(`\bptcp:(\d+)(?::(\S+))?`)
	// wildcardExplicitRe matches a literal `0.0.0.0:PORT`, bounded so it
	// does not match the tail of an unrelated dotted-quad like
	// `127.0.0.1:6641` or a template placeholder's trailing `:53`.
	wildcardExplicitRe = regexp.MustCompile(`(?:^|[\s"'=,])(0\.0\.0\.0):(\d{2,5})(?:[\s"'),]|$)`)
	// bareColonRe matches a value that is exactly `:PORT` with no address —
	// Go's "listen on every interface" shorthand.
	bareColonRe = regexp.MustCompile(`"(:(\d{2,5}))"`)
)

// scanBindSites reads path line by line and extracts every ptcp remote and
// literal wildcard bind. Comment lines are skipped: this package's own
// prose (e.g. "not a wildcard ptcp:6641") would otherwise read as a bind.
func scanBindSites(path string) ([]bindSite, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var sites []bindSite
	sc := bufio.NewScanner(f)
	lineNo := 0
	for sc.Scan() {
		lineNo++
		line := sc.Text()
		if strings.HasPrefix(strings.TrimSpace(line), "#") {
			continue
		}

		for _, m := range ptcpRe.FindAllStringSubmatch(line, -1) {
			port, perr := strconv.Atoi(m[1])
			if perr != nil {
				continue
			}
			bare := m[2] == ""
			wc := bare || listenerinventory.IsWildcardAddr(m[2])
			sites = append(sites, bindSite{
				file: path, line: lineNo, port: port,
				wildcard: wc, barePtcp: bare,
				raw: strings.TrimSpace(m[0]),
			})
		}
		for _, m := range wildcardExplicitRe.FindAllStringSubmatch(line, -1) {
			port, perr := strconv.Atoi(m[2])
			if perr != nil {
				continue
			}
			sites = append(sites, bindSite{
				file: path, line: lineNo, port: port,
				wildcard: true,
				raw:      m[1] + ":" + m[2],
			})
		}
		for _, m := range bareColonRe.FindAllStringSubmatch(line, -1) {
			port, perr := strconv.Atoi(m[2])
			if perr != nil {
				continue
			}
			sites = append(sites, bindSite{
				file: path, line: lineNo, port: port,
				wildcard: true,
				raw:      m[1],
			})
		}
	}
	return sites, sc.Err()
}

// checkBindSite applies the three listener-invariant rules to one bind site
// and returns a failure message naming the file, the line and the fix, or ""
// if the site is clean.
func checkBindSite(table *listenerinventory.Table, s bindSite, root string) string {
	rel := relTo(s.file, root)

	if s.barePtcp {
		return fmt.Sprintf(
			"%s:%d: bare `ptcp:%d` has no explicit bind address, so OVSDB "+
				"binds the wildcard by default — this is the F1 defect (OVN "+
				"NB/SB on the WAN). Fix: pin it, e.g. `ptcp:%d:127.0.0.1`.",
			rel, s.line, s.port, s.port)
	}

	if !table.KnownPort(s.port) {
		return fmt.Sprintf(
			"%s:%d: binds port %d (%s), which has no row in "+
				"docs/security/network-connections/README.md \"## 1. Inbound "+
				"Listeners\". Fix: add a row classifying its Scope, or remove "+
				"the listener.",
			rel, s.line, s.port, s.raw)
	}

	if !s.wildcard {
		return ""
	}
	for _, r := range table.RowsForPort(s.port) {
		if r.Scope != listenerinventory.ScopeCluster && r.Scope != listenerinventory.ScopeEncap {
			continue
		}
		if r.WildcardOK() {
			continue
		}
		return fmt.Sprintf(
			"%s:%d: binds port %d to the wildcard address (%s), but the "+
				"inventory classifies it %s and its row does not say it binds "+
				"the wildcard by design. Fix: bind the lan/vpc-plane address "+
				"instead, or add that justification to the inventory row.",
			rel, s.line, s.port, s.raw, r.Scope)
	}
	return ""
}
