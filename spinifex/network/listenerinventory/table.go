package listenerinventory

import (
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

// Scope classifies a listener's intended reach, matching the "Scope" column
// of the inventory table.
type Scope int

const (
	ScopeUnknown Scope = iota
	ScopeExternal
	ScopeCluster
	ScopeEncap
	ScopeGuest
	ScopeLocalhost
)

func (s Scope) String() string {
	switch s {
	case ScopeExternal:
		return "External"
	case ScopeCluster:
		return "Cluster"
	case ScopeEncap:
		return "Encap"
	case ScopeGuest:
		return "Guest"
	case ScopeLocalhost:
		return "Localhost"
	default:
		return "Unknown"
	}
}

// Row is one parsed data row of the inbound-listeners table.
type Row struct {
	RawPort  string // the Port cell verbatim, e.g. "500, 4500" or "169.254.169.254:80"
	Ports    []int  // numeric ports parsed out of RawPort; nil for non-numeric cells (ESP, dynamic)
	Addr     string // literal address prefix when RawPort is "ip:port"; empty otherwise
	Service  string
	Protocol string
	Scope    Scope
	Purpose  string
	Auth     string
}

// wildcardExceptionMarker is the exact phrase a row's Purpose or Auth column
// must contain to declare a wildcard-bind exception. This check is
// deliberately fail-closed: no other wording counts, including a mention of
// "wildcard" or "0.0.0.0" with no marker, and including a negated mention
// like "never the wildcard address". Both resolve to no exception, for the
// same reason — the marker is absent — rather than one relying on the
// absence of a negation word an author didn't think to use.
const wildcardExceptionMarker = "binds the wildcard by design"

// WildcardOK reports whether this row's own Purpose/Auth prose declares,
// via the exact marker above, that the listener binds the wildcard address
// by design. This is the doc-declared exception a bind-site or runtime
// check must honor rather than a hardcoded port list. The comparison folds
// case only — sentence-initial "Binds the wildcard by design." still
// counts — nothing else about the marker is fuzzy.
func (r Row) WildcardOK() bool {
	return strings.Contains(strings.ToLower(r.Purpose), wildcardExceptionMarker) ||
		strings.Contains(strings.ToLower(r.Auth), wildcardExceptionMarker)
}

// Table is the parsed inbound-listeners inventory.
type Table struct {
	Rows []Row
}

// ParseFile reads and parses the inventory table from the security doc at path.
func ParseFile(path string) (*Table, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("listenerinventory: read %s: %w", path, err)
	}
	return Parse(string(b))
}

const inboundListenersHeading = "## 1. Inbound Listeners"

// Parse extracts the inbound-listeners markdown table from content.
func Parse(content string) (*Table, error) {
	lines := strings.Split(content, "\n")
	start := -1
	for i, l := range lines {
		if strings.HasPrefix(strings.TrimSpace(l), inboundListenersHeading) {
			start = i
			break
		}
	}
	if start < 0 {
		return nil, fmt.Errorf("listenerinventory: no %q section found", inboundListenersHeading)
	}

	var headerSeen, sepSeen bool
	var t Table
	for _, l := range lines[start+1:] {
		trimmed := strings.TrimSpace(l)
		if !strings.HasPrefix(trimmed, "|") {
			if headerSeen {
				break // prose or a blank line ends the table
			}
			continue
		}
		if !headerSeen {
			headerSeen = true // the header row itself
			continue
		}
		if !sepSeen {
			sepSeen = true // the "|---|---|" separator row
			continue
		}
		if row, ok := parseRow(trimmed); ok {
			t.Rows = append(t.Rows, row)
		}
	}
	if len(t.Rows) == 0 {
		return nil, fmt.Errorf("listenerinventory: parsed zero rows from %q", inboundListenersHeading)
	}
	return &t, nil
}

func parseRow(line string) (Row, bool) {
	line = strings.TrimPrefix(line, "|")
	line = strings.TrimSuffix(line, "|")
	cells := strings.Split(line, "|")
	if len(cells) < 6 {
		return Row{}, false
	}
	for i := range cells {
		cells[i] = strings.TrimSpace(cells[i])
	}
	ports, addr := parsePortCell(cells[0])
	return Row{
		RawPort:  cells[0],
		Ports:    ports,
		Addr:     addr,
		Service:  cells[1],
		Protocol: cells[2],
		Scope:    parseScope(cells[3]),
		Purpose:  cells[4],
		Auth:     cells[5],
	}, true
}

var ipPortCellRe = regexp.MustCompile(`^(\d{1,3}(?:\.\d{1,3}){3}):(\d+)$`)

// parsePortCell handles every shape the Port column takes: a bare number, a
// comma-separated list ("500, 4500"), an address:port pair
// ("169.254.169.254:80"), an em-dash for a portless protocol (ESP), and
// free text ("socket / dynamic TCP") for a listener with no fixed port.
func parsePortCell(cell string) (ports []int, addr string) {
	cell = strings.TrimSpace(cell)
	if cell == "" || cell == "—" || cell == "-" {
		return nil, ""
	}
	if m := ipPortCellRe.FindStringSubmatch(cell); m != nil {
		if p, err := strconv.Atoi(m[2]); err == nil {
			return []int{p}, m[1]
		}
		return nil, ""
	}
	if !strings.ContainsAny(cell, "0123456789") {
		return nil, ""
	}
	for tok := range strings.SplitSeq(cell, ",") {
		tok = strings.TrimSpace(tok)
		p, err := strconv.Atoi(tok)
		if err != nil {
			continue // prose token in an otherwise numeric cell; skip rather than fail
		}
		ports = append(ports, p)
	}
	return ports, ""
}

func parseScope(cell string) Scope {
	lower := strings.ToLower(cell)
	switch {
	case strings.Contains(lower, "encap"):
		return ScopeEncap
	case strings.Contains(lower, "external"):
		return ScopeExternal
	case strings.Contains(lower, "guest"):
		return ScopeGuest
	case strings.Contains(lower, "localhost"):
		return ScopeLocalhost
	case strings.Contains(lower, "cluster"):
		return ScopeCluster
	default:
		return ScopeUnknown
	}
}

// RowsForPort returns every row whose Ports list contains port. More than
// one row can share a port — e.g. 53 is both northstar's External listener
// on the advertise address and vpcd's Guest-scope VPC DNS on a per-instance
// address — so callers must check Scope on each returned row rather than
// assume a 1:1 mapping.
func (t *Table) RowsForPort(port int) []Row {
	var out []Row
	for _, r := range t.Rows {
		if slices.Contains(r.Ports, port) {
			out = append(out, r)
		}
	}
	return out
}

// KnownPort reports whether port appears in any row of the table.
func (t *Table) KnownPort(port int) bool {
	return len(t.RowsForPort(port)) > 0
}

// IsWildcardAddr reports whether addr denotes "every interface", in either
// ss(8) or config-file notation: "0.0.0.0", "::", "*", "[::]", or the empty
// string (Go's bare ":PORT" listen form, address already stripped).
func IsWildcardAddr(addr string) bool {
	switch addr {
	case "0.0.0.0", "::", "*", "[::]", "":
		return true
	default:
		return false
	}
}

// RepoRoot returns the spinifex module root via `go env GOMOD`.
func RepoRoot() (string, error) {
	out, err := exec.Command("go", "env", "GOMOD").Output()
	if err != nil {
		return "", fmt.Errorf("listenerinventory: go env GOMOD: %w", err)
	}
	gomod := strings.TrimSpace(string(out))
	if gomod == "" || gomod == "/dev/null" {
		return "", fmt.Errorf("listenerinventory: not in a go module (GOMOD=%q)", gomod)
	}
	return strings.TrimSuffix(gomod, "/go.mod"), nil
}

// DocPath returns the standard location of the inbound-listener inventory
// relative to root.
func DocPath(root string) string {
	return root + "/docs/security/network-connections/README.md"
}
