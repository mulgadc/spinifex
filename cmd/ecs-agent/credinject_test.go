package main

import (
	"testing"

	"github.com/mulgadc/spinifex/spinifex/handlers/ecs/bus"
)

func TestTaskCredID(t *testing.T) {
	cases := []struct {
		name string
		as   bus.Assign
		want string
	}{
		{"no role", bus.Assign{TaskID: "t1"}, ""},
		{"role, no credID -> taskID", bus.Assign{TaskID: "t1", TaskRoleARN: "arn:role"}, "t1"},
		{"role + credID", bus.Assign{TaskID: "t1", CredID: "c9", TaskRoleARN: "arn:role"}, "c9"},
	}
	for _, tc := range cases {
		if got := taskCredID(&tc.as); got != tc.want {
			t.Errorf("%s: taskCredID = %q, want %q", tc.name, got, tc.want)
		}
	}
}

func TestWithCredEnv(t *testing.T) {
	base := map[string]string{"FOO": "bar"}

	// No credID -> env returned unchanged (same map).
	if got := withCredEnv(base, ""); got["FOO"] != "bar" || got["AWS_CONTAINER_CREDENTIALS_RELATIVE_URI"] != "" {
		t.Errorf("blank credID mutated env: %v", got)
	}

	got := withCredEnv(base, "cred-1")
	if got["AWS_CONTAINER_CREDENTIALS_RELATIVE_URI"] != "/v2/credentials/cred-1" {
		t.Errorf("missing relative-uri: %v", got)
	}
	if got["FOO"] != "bar" {
		t.Errorf("dropped existing env: %v", got)
	}
	// Original map must be untouched (copy semantics).
	if _, ok := base["AWS_CONTAINER_CREDENTIALS_RELATIVE_URI"]; ok {
		t.Errorf("source env mutated: %v", base)
	}
}

func TestSessionName(t *testing.T) {
	if got := sessionName("abc"); got != "ecs-abc" {
		t.Errorf("sessionName = %q", got)
	}
	long := sessionName(string(make([]byte, 100)))
	if len(long) != 64 {
		t.Errorf("sessionName not truncated to 64: len=%d", len(long))
	}
}
