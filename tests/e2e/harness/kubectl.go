//go:build e2e

package harness

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"
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
// apiserver refused the request, and the two explanations — a token it rejected
// versus one the exec plugin never produced — need different fixes.
//
// It probes an authenticated path rather than `/version`, which is anonymously
// readable and so returns 200 under either explanation, and it invokes the
// credential plugin directly so a plugin that failed is named rather than
// inferred from a missing header.
func (k *Kubectl) AuthDiagnostics(timeout time.Duration) string {
	var b strings.Builder

	b.WriteString("=== kubectl auth whoami ===\n")
	out, err := k.Run(timeout, "auth", "whoami")
	b.WriteString(out)
	if err != nil {
		b.WriteString("\nerror: " + err.Error() + "\n")
	}

	// kube-system is not anonymously readable, so a 200 here proves a credential
	// was both produced and accepted.
	b.WriteString("\n=== GET /api/v1/namespaces/kube-system ===\n")
	out, err = k.Run(timeout, "--v=8", "get", "--raw", "/api/v1/namespaces/kube-system")
	b.WriteString(out)
	if err != nil {
		b.WriteString("\nerror: " + err.Error() + "\n")
	}

	b.WriteString("\n=== credential plugin, invoked directly ===\n")
	b.WriteString(k.execPluginProbe(timeout))
	return b.String()
}

// execPluginProbe runs the kubeconfig's own exec credential plugin and reports
// whether it produced a token. The token itself is never printed — CI logs are
// retained for weeks and it is a live cluster credential.
func (k *Kubectl) execPluginProbe(timeout time.Duration) string {
	spec, err := k.Run(timeout, "config", "view", "--raw", "-o",
		"jsonpath={.users[0].user.exec.command}{range .users[0].user.exec.args[*]} {@}{end}")
	if err != nil {
		return "could not read exec block from kubeconfig: " + err.Error() + "\n" + spec
	}
	argv := strings.Fields(strings.TrimSpace(spec))
	if len(argv) == 0 {
		return "kubeconfig user has no exec credential plugin\n"
	}

	bin, err := exec.LookPath(argv[0])
	if err != nil {
		return "exec plugin " + argv[0] + " not on PATH: " + err.Error() + "\n"
	}
	cmd := exec.Command(bin, argv[1:]...) //nolint:gosec // argv comes from the kubeconfig this test wrote
	cmd.Env = append(os.Environ(), k.AWSEnv...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr

	var b strings.Builder
	fmt.Fprintf(&b, "argv: %s\n", strings.Join(argv, " "))
	runErr := runWithTimeout(cmd, timeout)
	fmt.Fprintf(&b, "exit: %v\n", runErr)
	fmt.Fprintf(&b, "stderr: %s\n", strings.TrimSpace(stderr.String()))

	var cred struct {
		Status struct {
			Token               string `json:"token"`
			ExpirationTimestamp string `json:"expirationTimestamp"`
		} `json:"status"`
	}
	if jsonErr := json.Unmarshal(stdout.Bytes(), &cred); jsonErr != nil {
		fmt.Fprintf(&b, "stdout did not parse as an ExecCredential: %v (%d bytes)\n",
			jsonErr, stdout.Len())
		return b.String()
	}
	fmt.Fprintf(&b, "token produced: %t (%d chars), expires: %q\n",
		cred.Status.Token != "", len(cred.Status.Token), cred.Status.ExpirationTimestamp)
	return b.String()
}

type timeoutError struct{}

func (*timeoutError) Error() string { return "kubectl timed out" }
