//go:build e2e

package harness

import (
	"bytes"
	"os"
	"os/exec"
	"testing"
	"time"
)

// Kubectl wraps `kubectl` invocations against a specific kubeconfig for the EKS
// data-plane scenarios. AWSEnv carries the credential-plugin environment
// (AWS_PROFILE / endpoint / CA) that the kubeconfig's `aws eks get-token` exec
// block needs — kubectl spawns it with this process's environment.
type Kubectl struct {
	Bin        string
	Kubeconfig string
	AWSEnv     []string
}

// NewKubectl locates kubectl on PATH (KUBECTL_BIN overrides) and SKIPS the
// calling test if it is absent — the Phase-2 data-plane subtests are optional
// where kubectl is not installed.
func NewKubectl(t *testing.T, kubeconfig string, awsEnv []string) *Kubectl {
	t.Helper()
	bin := os.Getenv("KUBECTL_BIN")
	if bin == "" {
		bin = "kubectl"
	}
	resolved, err := exec.LookPath(bin)
	if err != nil {
		t.Skipf("kubectl not found on PATH (%v); skipping data-plane subtest", err)
	}
	return &Kubectl{Bin: resolved, Kubeconfig: kubeconfig, AWSEnv: awsEnv}
}

// Run executes `kubectl --kubeconfig <kc> <args...>` and returns combined
// output. Errors are returned (not fatal) so callers can poll readiness.
func (k *Kubectl) Run(timeout time.Duration, args ...string) (string, error) {
	full := append([]string{"--kubeconfig", k.Kubeconfig}, args...)
	cmd := exec.Command(k.Bin, full...) //nolint:gosec // bin is LookPath-resolved, args test-controlled
	env := os.Environ()
	env = append(env, k.AWSEnv...)
	cmd.Env = env
	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	done := make(chan error, 1)
	if err := cmd.Start(); err != nil {
		return "", err
	}
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

// AuthDiagnostics reproduces the credential exchange behind a failing call
// without mutating anything. `Unauthorized` on its own says only that the
// apiserver refused the request; the verbose read-only probe says whether an
// Authorization header was sent at all, which separates a rejected token from
// a credential the exec plugin never produced.
func (k *Kubectl) AuthDiagnostics(timeout time.Duration) string {
	out, err := k.Run(timeout, "--v=8", "get", "--raw", "/version")
	if err != nil {
		return out + "\nprobe error: " + err.Error()
	}
	return out
}

type timeoutError struct{}

func (*timeoutError) Error() string { return "kubectl timed out" }
