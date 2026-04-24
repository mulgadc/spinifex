package admin

import (
	"os"
	"path/filepath"
	"testing"
)

func TestValidateEmail(t *testing.T) {
	cases := []struct {
		in      string
		wantErr bool
	}{
		{"admin@example.com", false},
		{"foo.bar+tag@sub.example.co.uk", false},
		{"  ok@example.io  ", false}, // trimmed
		{"", true},
		{"   ", true},
		{"nodomain", true},
		{"missing@tld", true},       // no dot after @
		{"@example.com", true},      // empty local part
		{"user@.com", true},         // dot immediately after @
		{"user@@example.com", true}, // double @
		{"user @example.com", true}, // whitespace in local
		{"user@exa mple.com", true}, // whitespace in domain
	}
	for _, c := range cases {
		err := ValidateEmail(c.in)
		if c.wantErr && err == nil {
			t.Errorf("ValidateEmail(%q) = nil, want error", c.in)
		}
		if !c.wantErr && err != nil {
			t.Errorf("ValidateEmail(%q) = %v, want nil", c.in, err)
		}
	}
}

func TestReadOperatorEmail(t *testing.T) {
	dir := t.TempDir()

	write := func(name, body string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
		return p
	}

	t.Run("nonexistent file", func(t *testing.T) {
		if got := ReadOperatorEmail(filepath.Join(dir, "missing.toml")); got != "" {
			t.Errorf("want empty string for missing file, got %q", got)
		}
	})

	t.Run("no operator section", func(t *testing.T) {
		p := write("none.toml", `version = "2"
[network]
external_mode = "pool"
`)
		if got := ReadOperatorEmail(p); got != "" {
			t.Errorf("want empty string, got %q", got)
		}
	})

	t.Run("operator section with email", func(t *testing.T) {
		p := write("ok.toml", `version = "2"

[operator]
email = "admin@example.com"

[network]
external_mode = "pool"
`)
		if got := ReadOperatorEmail(p); got != "admin@example.com" {
			t.Errorf("want admin@example.com, got %q", got)
		}
	})

	t.Run("email scalar in wrong section ignored", func(t *testing.T) {
		// A stray "email = ..." outside [operator] must not be returned.
		p := write("stray.toml", `version = "2"
email = "wrong@example.com"

[some_other]
email = "alsowrong@example.com"
`)
		if got := ReadOperatorEmail(p); got != "" {
			t.Errorf("want empty (stray scalars ignored), got %q", got)
		}
	})

	t.Run("operator section trailing", func(t *testing.T) {
		// Section that appears last in the file.
		p := write("last.toml", `version = "2"

[network]
external_mode = "pool"

[operator]
email = "late@example.com"
`)
		if got := ReadOperatorEmail(p); got != "late@example.com" {
			t.Errorf("want late@example.com, got %q", got)
		}
	})

	t.Run("operator section first, other section after", func(t *testing.T) {
		// Ensures the scanner correctly re-enters "not in operator" state on
		// the next section header.
		p := write("mixed.toml", `[operator]
email = "first@example.com"

[nodes.node1]
region = "ap-southeast-2"
`)
		if got := ReadOperatorEmail(p); got != "first@example.com" {
			t.Errorf("want first@example.com, got %q", got)
		}
	})
}
