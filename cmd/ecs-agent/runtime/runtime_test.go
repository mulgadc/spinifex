package runtime

import (
	"testing"
)

func TestNormalizeRef(t *testing.T) {
	cases := map[string]string{
		"nginx:alpine":      "docker.io/library/nginx:alpine",
		"nginx":             "docker.io/library/nginx:latest",
		"ghcr.io/org/app:1": "ghcr.io/org/app:1",
		"123456789012.dkr.ecr.ap-southeast-2.spinifex.internal/app:1": "123456789012.dkr.ecr.ap-southeast-2.spinifex.internal/app:1",
	}
	for in, want := range cases {
		got, err := normalizeRef(in)
		if err != nil {
			t.Errorf("normalizeRef(%q): %v", in, err)
			continue
		}
		if got != want {
			t.Errorf("normalizeRef(%q) = %q, want %q", in, got, want)
		}
	}
	if _, err := normalizeRef("Bad::Ref"); err == nil {
		t.Error("normalizeRef accepted invalid ref")
	}
}
