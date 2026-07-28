// Package guestenv reads the static settings an in-guest binary is configured
// with at launch. Every Spinifex guest agent is handed a KEY=value file by
// cloud-init (/etc/spinifex-<service>/agent.env) and reads the same settings
// from the process environment, so this is the one place that shape lives.
package guestenv

import (
	"bufio"
	"os"
	"strings"
)

// Loader resolves one setting at a time from an env file, letting a real
// environment variable win — so an operator or a test can override what
// cloud-init wrote without editing it.
type Loader map[string]string

// Load parses the KEY=value file at path. A missing or unreadable file yields
// an empty Loader rather than an error: the settings are all optional, and a
// guest whose file never arrived must still start far enough to report why.
func Load(path string) Loader {
	out := Loader{}
	f, err := os.Open(path)
	if err != nil {
		return out
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, val, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		out[strings.TrimSpace(key)] = strings.TrimSpace(val)
	}
	return out
}

// Get returns the process environment's value for key, falling back to the
// file's. An empty environment value is treated as unset so an exported-but-
// blank variable does not mask a delivered setting.
func (l Loader) Get(key string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return l[key]
}
