package cmd_test

import (
	"bytes"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/mulgadc/spinifex/cmd/spinifex/cmd"
	"github.com/pterm/pterm"
)

func TestOutputStylingEnabled(t *testing.T) {
	tests := []struct {
		name     string
		noColor  string
		terminal bool
		want     bool
	}{
		{name: "terminal keeps colour", terminal: true, want: true},
		{name: "pipe drops colour", terminal: false, want: false},
		{name: "NO_COLOR drops colour on a terminal", noColor: "1", terminal: true, want: false},
		{name: "any non-empty NO_COLOR counts, even 0", noColor: "0", terminal: true, want: false},
		{name: "NO_COLOR on a pipe", noColor: "1", terminal: false, want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := cmd.OutputStylingEnabled(tt.noColor, tt.terminal); got != tt.want {
				t.Fatalf("OutputStylingEnabled(%q, %v) = %v, want %v", tt.noColor, tt.terminal, got, tt.want)
			}
		})
	}
}

func TestIsTerminalFalseForNonTTYWriters(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	defer w.Close()

	f, err := os.CreateTemp(t.TempDir(), "out")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	for name, wr := range map[string]io.Writer{"pipe": w, "regular file": f, "buffer": &bytes.Buffer{}} {
		if cmd.IsTerminal(wr) {
			t.Errorf("IsTerminal(%s) = true, want false", name)
		}
	}
}

// renderNodesTable renders a table shaped like `spx get nodes` through the same
// pterm.DefaultTable the CLI uses, after applying the styling decision.
func renderNodesTable(t *testing.T, noColor string, terminal bool) string {
	t.Helper()
	t.Cleanup(func() { cmd.SetOutputStyling(true) })
	cmd.SetOutputStyling(cmd.OutputStylingEnabled(noColor, terminal))

	var buf bytes.Buffer
	data := pterm.TableData{
		{"NAME", "STATUS", "IP"},
		{"node1", "Ready", "10.0.0.1"},
		{"node-long-name", "NotReady", "10.0.0.22"},
	}
	if err := pterm.DefaultTable.WithHasHeader().WithLeftAlignment().WithData(data).WithWriter(&buf).Render(); err != nil {
		t.Fatalf("render: %v", err)
	}
	return buf.String()
}

func TestTableOutputPlainWhenNotTerminal(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	defer w.Close()

	out := renderNodesTable(t, "", cmd.IsTerminal(w))
	assertPlainAlignedTable(t, out)
}

func TestTableOutputPlainWithNoColorOnTerminal(t *testing.T) {
	out := renderNodesTable(t, "1", true)
	assertPlainAlignedTable(t, out)
}

func TestTableOutputStyledOnTerminal(t *testing.T) {
	out := renderNodesTable(t, "", true)
	if !strings.Contains(out, "\x1b[") {
		t.Fatalf("table on a terminal carries no ANSI styling, so the plain-output tests prove nothing:\n%q", out)
	}
}

func assertPlainAlignedTable(t *testing.T, out string) {
	t.Helper()
	if strings.ContainsRune(out, '\x1b') {
		t.Fatalf("output contains an escape byte:\n%q", out)
	}

	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	var rows []string
	for _, l := range lines {
		if strings.Contains(l, "node") || strings.HasPrefix(l, "NAME") {
			rows = append(rows, l)
		}
	}
	if len(rows) != 3 {
		t.Fatalf("want header and two node rows, got %d:\n%s", len(rows), out)
	}

	// Columns line up only if the width calculation saw the same bytes it printed.
	statusCol := strings.Index(rows[0], "STATUS")
	ipCol := strings.Index(rows[0], "IP")
	for _, row := range rows[1:] {
		fields := strings.Fields(row)
		var status string
		for _, f := range fields {
			if strings.Contains(f, "Ready") {
				status = f
			}
		}
		if got := strings.Index(row, status); got != statusCol {
			t.Errorf("STATUS column at %d in %q, header at %d", got, row, statusCol)
		}
		if got := strings.Index(row, "10.0.0."); got != ipCol {
			t.Errorf("IP column at %d in %q, header at %d", got, row, ipCol)
		}
	}

	// grep -w Ready must match: the byte before the word is not a word character.
	ready := strings.Index(rows[1], "Ready")
	if ready <= 0 || rows[1][ready-1] != ' ' {
		t.Errorf("Ready is not preceded by a space in %q", rows[1])
	}
}
