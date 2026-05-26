// Package manifestcheck loads and validates spinifex/docs/service-interfaces.yaml.
//
// The validator is documentation-only at this stage of Bead 1: it confirms
// the manifest parses, that every declared service/suite path exists on
// disk, and that cross-references (suites.covers → services, fixtures →
// services) resolve. It does not yet enforce that subjects in the manifest
// match subjects in source — that's Bead 5 (e2e-manifest-drift-lint).
package manifestcheck

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"

	"gopkg.in/yaml.v3"
)

type Manifest struct {
	Version  int                `yaml:"version"`
	Services map[string]Service `yaml:"services"`
	Fixtures map[string]Fixture `yaml:"fixtures"`
	Suites   map[string]Suite   `yaml:"suites"`
}

type Service struct {
	Path            string   `yaml:"path"`
	AdditionalPaths []string `yaml:"additional_paths,omitempty"`
	Subscribes      []string `yaml:"subscribes"`
	Publishes       []string `yaml:"publishes"`
	DependsOn       []string `yaml:"depends_on"`
	CoversNoSuite   bool     `yaml:"covers_no_suite,omitempty"`
}

type Fixture struct {
	Services []string `yaml:"services"`
}

type Suite struct {
	Path   string   `yaml:"path"`
	Covers []string `yaml:"covers"`
}

func Load(path string) (*Manifest, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	var m Manifest
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&m); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	return &m, nil
}

// Validate checks structural invariants of the manifest. repoRoot is the
// directory paths are resolved against (typically the spinifex repo root).
// Returns one error per violation; the caller decides how to report them.
func Validate(m *Manifest, repoRoot string) []error {
	var errs []error

	if m.Version != 1 {
		errs = append(errs, fmt.Errorf("version: want 1, got %d", m.Version))
	}
	if len(m.Services) == 0 {
		errs = append(errs, fmt.Errorf("services: must declare at least one service"))
	}

	subjectRE := regexp.MustCompile(`^[a-z][a-z0-9]*(\.[a-zA-Z][a-zA-Z0-9_-]*|\.\*)+$`)

	for name, svc := range m.Services {
		if svc.Path == "" {
			errs = append(errs, fmt.Errorf("services.%s: path is required", name))
		} else if !dirExists(filepath.Join(repoRoot, svc.Path)) {
			errs = append(errs, fmt.Errorf("services.%s: path %q does not exist", name, svc.Path))
		}
		for _, p := range svc.AdditionalPaths {
			if !dirExists(filepath.Join(repoRoot, p)) {
				errs = append(errs, fmt.Errorf("services.%s: additional_paths %q does not exist", name, p))
			}
		}
		errs = append(errs, validateSubjects(name+".subscribes", svc.Subscribes, subjectRE)...)
		errs = append(errs, validateSubjects(name+".publishes", svc.Publishes, subjectRE)...)
		errs = append(errs, validateSubjects(name+".depends_on", svc.DependsOn, subjectRE)...)
	}

	for fxName, fx := range m.Fixtures {
		for _, svcName := range fx.Services {
			if _, ok := m.Services[svcName]; !ok {
				errs = append(errs, fmt.Errorf("fixtures.%s: references unknown service %q", fxName, svcName))
			}
		}
	}

	for suiteName, suite := range m.Suites {
		if suite.Path == "" {
			errs = append(errs, fmt.Errorf("suites.%s: path is required", suiteName))
		} else if !dirExists(filepath.Join(repoRoot, suite.Path)) {
			errs = append(errs, fmt.Errorf("suites.%s: path %q does not exist", suiteName, suite.Path))
		}
		if len(suite.Covers) == 0 {
			errs = append(errs, fmt.Errorf("suites.%s: covers must list at least one service", suiteName))
		}
		for _, svcName := range suite.Covers {
			svc, ok := m.Services[svcName]
			if !ok {
				errs = append(errs, fmt.Errorf("suites.%s.covers: references unknown service %q", suiteName, svcName))
				continue
			}
			if svc.CoversNoSuite {
				errs = append(errs, fmt.Errorf("suites.%s.covers: service %q is marked covers_no_suite", suiteName, svcName))
			}
		}
	}

	sort.Slice(errs, func(i, j int) bool { return errs[i].Error() < errs[j].Error() })
	return errs
}

func validateSubjects(ctx string, subjects []string, re *regexp.Regexp) []error {
	var errs []error
	seen := map[string]struct{}{}
	for _, s := range subjects {
		if !re.MatchString(s) {
			errs = append(errs, fmt.Errorf("%s: %q is not a valid NATS subject", ctx, s))
		}
		if _, dup := seen[s]; dup {
			errs = append(errs, fmt.Errorf("%s: duplicate subject %q", ctx, s))
		}
		seen[s] = struct{}{}
	}
	return errs
}

func dirExists(p string) bool {
	info, err := os.Stat(p)
	return err == nil && info.IsDir()
}
