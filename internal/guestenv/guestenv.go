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

type Loader map[string]string

// A missing or unreadable file yields an empty Loader rather than an error: the
// settings are all optional, and a guest whose file never arrived must still
// start far enough to report why.
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

// A real environment variable wins, so an operator or test can override what
// cloud-init wrote. An empty value is treated as unset rather than as a mask.
func (l Loader) Get(key string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return l[key]
}
