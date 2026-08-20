package migrate

import (
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"
)

// ConfigVersionReader reads and writes the schema version for a config file.
type ConfigVersionReader interface {
	ReadVersion(path string) (int, error)
	WriteVersion(path string, version int) error
}

// TOMLVersionReader reads/writes the version field in TOML config files.
// Handles both legacy string values ("1.0" → 1) and integer values (2 → 2).
// New writes always use quoted strings (version = "2").
type TOMLVersionReader struct{}

var _ ConfigVersionReader = (*TOMLVersionReader)(nil)

var tomlVersionRe = regexp.MustCompile(`(?m)^version\s*=\s*(?:"([^"]+)"|(\d+))\s*$`)

func (r *TOMLVersionReader) ReadVersion(path string) (int, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}

	match := tomlVersionRe.FindSubmatch(data)
	if match == nil {
		return 0, nil // no version field
	}

	var raw string
	if len(match[1]) > 0 {
		raw = string(match[1]) // quoted string like "1.0"
	} else {
		raw = string(match[2]) // bare integer like 2
	}

	// Handle "1.0" style — take the integer part before the dot.
	if idx := strings.Index(raw, "."); idx >= 0 {
		raw = raw[:idx]
	}

	v, err := strconv.Atoi(raw)
	if err != nil {
		return 0, fmt.Errorf("parse TOML version %q: %w", raw, err)
	}
	return v, nil
}

func (r *TOMLVersionReader) WriteVersion(path string, version int) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}

	replacement := fmt.Sprintf("version = \"%d\"", version)

	if tomlVersionRe.Match(data) {
		data = tomlVersionRe.ReplaceAll(data, []byte(replacement))
	} else {
		// Prepend version line if not present.
		data = append([]byte(replacement+"\n"), data...)
	}

	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, info.Mode())
}

// NATSConfVersionReader reads/writes the schema version for nats.conf files.
// Version is tracked via a comment on the first line: # spinifex-config-version: 1
// If absent, the file is treated as version 0 (pre-versioning).
type NATSConfVersionReader struct{}

var _ ConfigVersionReader = (*NATSConfVersionReader)(nil)

var natsVersionRe = regexp.MustCompile(`^#\s*spinifex-config-version:\s*(\d+)\s*$`)

func (r *NATSConfVersionReader) ReadVersion(path string) (int, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}

	// Check first line only.
	firstLine, _, _ := strings.Cut(string(data), "\n")
	match := natsVersionRe.FindStringSubmatch(firstLine)
	if match == nil {
		return 0, nil // no version marker
	}

	v, err := strconv.Atoi(match[1])
	if err != nil {
		return 0, fmt.Errorf("parse NATS conf version %q: %w", match[1], err)
	}
	return v, nil
}

func (r *NATSConfVersionReader) WriteVersion(path string, version int) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}

	versionLine := fmt.Sprintf("# spinifex-config-version: %d", version)
	content := string(data)

	// Check if first line already has a version marker.
	lines := strings.SplitN(content, "\n", 2)
	if natsVersionRe.MatchString(lines[0]) {
		// Replace existing version line.
		lines[0] = versionLine
		content = strings.Join(lines, "\n")
	} else {
		// Prepend version line.
		content = versionLine + "\n" + content
	}

	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	return os.WriteFile(path, []byte(content), info.Mode())
}
