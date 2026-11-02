package agent

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/mulgadc/spinifex/internal/stsauth"

	ecsv1 "github.com/mulgadc/spinifex/contracts/ecs/v1"
)

func TestTaskCredID(t *testing.T) {
	cases := []struct {
		name string
		as   ecsv1.Assign
		want string
	}{
		{"no role", ecsv1.Assign{TaskID: "t1"}, ""},
		{"role, no credID -> none", ecsv1.Assign{TaskID: "t1", TaskRoleARN: "arn:role"}, ""},
		{"role + credID", ecsv1.Assign{TaskID: "t1", CredID: "c9", TaskRoleARN: "arn:role"}, "c9"},
	}
	for _, tc := range cases {
		if got := taskCredID(&tc.as); got != tc.want {
			t.Errorf("%s: taskCredID = %q, want %q", tc.name, got, tc.want)
		}
	}
}

// The task ID is public (it is the task ARN suffix), so naming another task's ID
// at the credential endpoint must not yield that task's role.
func TestRegisterTaskCreds_TaskIDDoesNotServeCredentials(t *testing.T) {
	c, _ := newTestCredEndpoint(stsauth.Credentials{AccessKeyID: "AKIA", SecretAccessKey: "secret"})
	a := Agent{cred: c}
	get := func(id string) int {
		rr := httptest.NewRecorder()
		c.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, credRelativeURI(id), nil))
		return rr.Code
	}

	legacy := ecsv1.Assign{TaskID: "victim-task", TaskRoleARN: "arn:aws:iam::000000000010:role/high"}
	if got := a.registerTaskCreds(&legacy); got != "" {
		t.Errorf("assign without CredID registered credID %q", got)
	}
	if code := get("victim-task"); code != http.StatusNotFound {
		t.Errorf("GET by task ID of CredID-less assign = %d, want 404", code)
	}

	minted := ecsv1.Assign{TaskID: "victim-2", CredID: "secret-cred", TaskRoleARN: "arn:aws:iam::000000000010:role/high"}
	if got := a.registerTaskCreds(&minted); got != "secret-cred" {
		t.Fatalf("registerTaskCreds = %q, want secret-cred", got)
	}
	if code := get("victim-2"); code != http.StatusNotFound {
		t.Errorf("GET by task ID = %d, want 404", code)
	}
	if code := get("secret-cred"); code != http.StatusOK {
		t.Errorf("GET by CredID = %d, want 200", code)
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
