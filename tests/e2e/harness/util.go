//go:build e2e

package harness

import "strings"

// ShellQuote wraps s in single quotes and escapes embedded single quotes, so
// that shell metacharacters in interface names or peer addresses are treated
// as literals on the remote shell.
func ShellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
