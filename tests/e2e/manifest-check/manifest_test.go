package manifestcheck

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// repoRoot resolves the spinifex repo root from the test file's location.
// tests/e2e/manifest-check/ → spinifex repo root is three dirs up.
func repoRoot(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	return filepath.Join(filepath.Dir(file), "..", "..", "..")
}

func TestLoadAndValidate_RealManifest(t *testing.T) {
	root := repoRoot(t)
	path := filepath.Join(root, "docs", "service-interfaces.yaml")
	m, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if errs := Validate(m, root); len(errs) > 0 {
		for _, e := range errs {
			t.Errorf("validate: %v", e)
		}
	}
}

func writeTmp(t *testing.T, content string) string {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "manifest.yaml")
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestValidate_VersionMismatch(t *testing.T) {
	root := repoRoot(t)
	m := &Manifest{
		Version: 2,
		Services: map[string]Service{
			"awsgw": {Path: "spinifex/services/awsgw", Subscribes: []string{}, Publishes: []string{}, DependsOn: []string{}},
		},
	}
	errs := Validate(m, root)
	if !containsErr(errs, "version: want 1") {
		t.Errorf("expected version error, got %v", errs)
	}
}

func TestValidate_MissingServicePath(t *testing.T) {
	root := repoRoot(t)
	m := &Manifest{
		Version: 1,
		Services: map[string]Service{
			"phantom": {Path: "spinifex/services/does-not-exist"},
		},
	}
	errs := Validate(m, root)
	if !containsErr(errs, "phantom: path") {
		t.Errorf("expected missing-path error, got %v", errs)
	}
}

func TestValidate_BadSubject(t *testing.T) {
	root := repoRoot(t)
	m := &Manifest{
		Version: 1,
		Services: map[string]Service{
			"awsgw": {
				Path:       "spinifex/services/awsgw",
				Subscribes: []string{"NoDots", "ec2..bad", "ec2.Good"},
			},
		},
	}
	errs := Validate(m, root)
	bad := 0
	for _, e := range errs {
		if strings.Contains(e.Error(), "is not a valid NATS subject") {
			bad++
		}
	}
	if bad < 2 {
		t.Errorf("expected ≥2 subject errors, got %d (%v)", bad, errs)
	}
}

func TestValidate_DuplicateSubject(t *testing.T) {
	root := repoRoot(t)
	m := &Manifest{
		Version: 1,
		Services: map[string]Service{
			"awsgw": {
				Path:      "spinifex/services/awsgw",
				Publishes: []string{"ec2.RunInstances", "ec2.RunInstances"},
			},
		},
	}
	errs := Validate(m, root)
	if !containsErr(errs, "duplicate subject") {
		t.Errorf("expected duplicate-subject error, got %v", errs)
	}
}

func TestValidate_SuiteUnknownService(t *testing.T) {
	root := repoRoot(t)
	m := &Manifest{
		Version: 1,
		Services: map[string]Service{
			"awsgw": {Path: "spinifex/services/awsgw"},
		},
		Suites: map[string]Suite{
			"e2e-cert": {
				Path:   "spinifex/tests/e2e/cert",
				Covers: []string{"awsgw", "ghost_service"},
			},
		},
	}
	errs := Validate(m, root)
	if !containsErr(errs, `unknown service "ghost_service"`) {
		t.Errorf("expected unknown-service error, got %v", errs)
	}
}

func TestValidate_CoversNoSuiteConflict(t *testing.T) {
	root := repoRoot(t)
	m := &Manifest{
		Version: 1,
		Services: map[string]Service{
			"spinifexui": {Path: "spinifex/services/spinifexui", CoversNoSuite: true},
		},
		Suites: map[string]Suite{
			"e2e-cert": {
				Path:   "spinifex/tests/e2e/cert",
				Covers: []string{"spinifexui"},
			},
		},
	}
	errs := Validate(m, root)
	if !containsErr(errs, "covers_no_suite") {
		t.Errorf("expected covers_no_suite conflict, got %v", errs)
	}
}

func TestValidate_FixtureUnknownService(t *testing.T) {
	root := repoRoot(t)
	m := &Manifest{
		Version: 1,
		Services: map[string]Service{
			"awsgw": {Path: "spinifex/services/awsgw"},
		},
		Fixtures: map[string]Fixture{
			"EnsureAMI": {Services: []string{"awsgw", "ghost"}},
		},
	}
	errs := Validate(m, root)
	if !containsErr(errs, `unknown service "ghost"`) {
		t.Errorf("expected unknown-service in fixture, got %v", errs)
	}
}

func TestLoad_UnknownField(t *testing.T) {
	bad := `
version: 1
services:
  awsgw:
    path: spinifex/services/awsgw
    nonsense_field: true
`
	if _, err := Load(writeTmp(t, bad)); err == nil {
		t.Fatal("expected unknown-field error, got nil")
	}
}

func containsErr(errs []error, sub string) bool {
	for _, e := range errs {
		if strings.Contains(e.Error(), sub) {
			return true
		}
	}
	return false
}
