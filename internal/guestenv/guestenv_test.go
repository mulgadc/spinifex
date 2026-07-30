package guestenv

import (
	"os"
	"path/filepath"
	"testing"
)

func writeEnv(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "agent.env")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write env file: %v", err)
	}
	return path
}

func TestLoad_ParsesPairsAndSkipsNoise(t *testing.T) {
	l := Load(writeEnv(t, "# comment\n\nA=1\n  B = two \nnot-a-pair\nC=with=equals\n"))

	want := map[string]string{"A": "1", "B": "two", "C": "with=equals"}
	for k, v := range want {
		if got := l[k]; got != v {
			t.Errorf("Load()[%q] = %q, want %q", k, got, v)
		}
	}
	if len(l) != len(want) {
		t.Errorf("Load() has %d entries, want %d: %v", len(l), len(want), l)
	}
}

func TestLoad_MissingFileIsEmpty(t *testing.T) {
	if l := Load(filepath.Join(t.TempDir(), "absent.env")); len(l) != 0 {
		t.Errorf("Load(absent) = %v, want empty", l)
	}
}

func TestGet_EnvironmentOverridesFile(t *testing.T) {
	l := Load(writeEnv(t, "GUESTENV_TEST_KEY=from-file\nGUESTENV_TEST_BLANK=from-file\n"))

	t.Setenv("GUESTENV_TEST_KEY", "from-env")
	if got := l.Get("GUESTENV_TEST_KEY"); got != "from-env" {
		t.Errorf("Get() = %q, want the environment value", got)
	}

	// An exported-but-blank variable must not mask the delivered setting.
	t.Setenv("GUESTENV_TEST_BLANK", "")
	if got := l.Get("GUESTENV_TEST_BLANK"); got != "from-file" {
		t.Errorf("Get() = %q, want the file value", got)
	}

	if got := l.Get("GUESTENV_TEST_ABSENT"); got != "" {
		t.Errorf("Get(absent) = %q, want empty", got)
	}
}
