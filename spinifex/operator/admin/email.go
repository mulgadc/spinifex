package admin

import (
	"fmt"
	"os"
	"regexp"
	"strings"

	toml "github.com/pelletier/go-toml/v2"
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
// returning "" on any error or missing section.
func ReadOperatorEmail(tomlPath string) string {
	raw, err := os.ReadFile(tomlPath)
	if err != nil {
		return ""
	}

	var cfg struct {
		Operator struct {
			Email string `toml:"email"`
		} `toml:"operator"`
	}
	if err := toml.Unmarshal(raw, &cfg); err != nil {
		return ""
	}
	return cfg.Operator.Email
}
