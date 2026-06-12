package admin

import (
	"bufio"
	"fmt"
	"os"
	"regexp"
	"strings"
)

// Deliberately loose — we want to catch obvious typos (missing @, missing
// dot after @, whitespace) without pretending to validate RFC 5321.
// Identical check applied in the installer TUI and in `spx admin init --email`.
var emailRE = regexp.MustCompile(`^[^\s@]+@[^\s@]+\.[^\s@]+$`)

// ValidateEmail returns nil if addr is a plausible email address, or an
// error describing the failure. Empty strings fail.
func ValidateEmail(addr string) error {
	addr = strings.TrimSpace(addr)
	if addr == "" {
		return fmt.Errorf("email is required")
	}
	if !emailRE.MatchString(addr) {
		return fmt.Errorf("%q is not a valid email address", addr)
	}
	return nil
}

// ReadOperatorEmail extracts the [operator].email scalar from spinifex.toml,
// returning "" on any error or missing section. Uses a text scan, not a full TOML parser.
func ReadOperatorEmail(tomlPath string) string {
	f, err := os.Open(tomlPath)
	if err != nil {
		return ""
	}
	defer f.Close()

	keyRE := regexp.MustCompile(`^\s*email\s*=\s*"([^"]*)"`)
	sc := bufio.NewScanner(f)
	inOperator := false
	for sc.Scan() {
		line := sc.Text()
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "[") {
			inOperator = trimmed == "[operator]"
			continue
		}
		if !inOperator {
			continue
		}
		if m := keyRE.FindStringSubmatch(line); m != nil {
			return m[1]
		}
	}
	return ""
}
