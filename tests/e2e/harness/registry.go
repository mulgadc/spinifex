//go:build e2e

package harness

import (
	"bytes"
	"net"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// ExternalCLI wraps an OCI client binary (docker/crane/skopeo) for the ECR
// data-plane subtests. Env is appended to the process environment (e.g.
// DOCKER_CONFIG to isolate the auth file).
type ExternalCLI struct {
	Name string
	Bin  string
	Env  []string
}

// newExternalCLI locates bin on PATH (overridable via <NAME>_BIN, upper-cased)
// and SKIPS the calling test when absent — the data-plane subtests are optional
// on runners without the client installed.
func newExternalCLI(t *testing.T, name string, env []string) *ExternalCLI {
	t.Helper()
	bin := os.Getenv(strings.ToUpper(name) + "_BIN")
	if bin == "" {
		bin = name
	}
	resolved, err := exec.LookPath(bin)
	if err != nil {
		t.Skipf("%s not found on PATH (%v); skipping data-plane subtest", name, err)
	}
	return &ExternalCLI{Name: name, Bin: resolved, Env: env}
}

// NewDocker / NewCrane / NewSkopeo build wrappers for each client, skipping the
// test if the binary is missing. dockerConfig isolates the docker auth file so
// crane/skopeo can reuse it via DOCKER_CONFIG without touching ~/.docker.
func NewDocker(t *testing.T, dockerConfig string) *ExternalCLI {
	return newExternalCLI(t, "docker", []string{"DOCKER_CONFIG=" + dockerConfig})
}

func NewCrane(t *testing.T, dockerConfig string) *ExternalCLI {
	return newExternalCLI(t, "crane", []string{"DOCKER_CONFIG=" + dockerConfig})
}

func NewSkopeo(t *testing.T, dockerConfig string) *ExternalCLI {
	return newExternalCLI(t, "skopeo", []string{"REGISTRY_AUTH_FILE=" + dockerConfig + "/config.json"})
}

// Run executes the client with args and returns combined output. Errors are
// returned (not fatal) so callers can assert on output or poll.
func (c *ExternalCLI) Run(timeout time.Duration, args ...string) (string, error) {
	cmd := exec.Command(c.Bin, args...) //nolint:gosec // bin is LookPath-resolved, args test-controlled
	cmd.Env = append(os.Environ(), c.Env...)
	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	if err := cmd.Start(); err != nil {
		return "", err
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		return buf.String(), err
	case <-time.After(timeout):
		_ = cmd.Process.Kill()
		<-done
		return buf.String(), &timeoutError{}
	}
}

// RunStdin is Run with data piped to the process stdin (docker login
// --password-stdin).
func (c *ExternalCLI) RunStdin(timeout time.Duration, stdin string, args ...string) (string, error) {
	cmd := exec.Command(c.Bin, args...) //nolint:gosec // bin is LookPath-resolved, args test-controlled
	cmd.Env = append(os.Environ(), c.Env...)
	cmd.Stdin = strings.NewReader(stdin)
	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	if err := cmd.Start(); err != nil {
		return "", err
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		return buf.String(), err
	case <-time.After(timeout):
		_ = cmd.Process.Kill()
		<-done
		return buf.String(), &timeoutError{}
	}
}

// RequireRegistryResolves SKIPS the calling test unless host resolves to an
// address. host may carry a :port suffix (stripped before lookup). The
// data-plane subtests need {account}.dkr.ecr.{region}.{suffix} to reach the
// awsgw bind IP (Route53 in prod, /etc/hosts in dev); without it the client
// cannot connect.
func RequireRegistryResolves(t *testing.T, host string) {
	t.Helper()
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	if _, err := net.LookupHost(host); err != nil {
		t.Skipf("registry host %q does not resolve (%v); seed /etc/hosts or DNS", host, err)
	}
}
