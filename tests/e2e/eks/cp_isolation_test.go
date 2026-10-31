//go:build e2e

package eks

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mulgadc/spinifex/tests/e2e/harness"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const cpIsolationNodegroup = "cpiso-e2e-ng"

// runCPIsolation proves control-plane hosts never register as Nodes, as on AWS:
// only nodegroup workers are listed, a DaemonSet tolerating every taint runs on
// workers only, metrics-server is Ready, and logs work for every kube-system pod.
func runCPIsolation(t *testing.T, c *harness.AWSClient, env *harness.Env, artifacts string, fx *clusterFixture) {
	nodeRoleArn, _ := ensureNodeRole(t, c, fx)
	createWorkerNodegroup(t, c, fx, cpIsolationNodegroup, nodeRoleArn, nil)

	kcPath := writeKubeconfig(t, artifacts, fx.Cluster)
	kc := harness.NewKubectl(t, kcPath, getTokenEnv(t, env))

	t.Run("OnlyWorkersAreNodes", func(t *testing.T) {
		out, err := kc.Run(30*time.Second, "get", "nodes",
			"-l", "node-role.kubernetes.io/control-plane", "-o", "name")
		require.NoErrorf(t, err, "get control-plane nodes:\n%s", out)
		assert.Emptyf(t, strings.TrimSpace(out), "control-plane hosts must not register as Nodes:\n%s", out)

		out, err = kc.Run(30*time.Second, "get", "nodes", "-o", "jsonpath={range .items[*]}{.metadata.name}{\"\\n\"}{end}")
		require.NoErrorf(t, err, "get nodes:\n%s", out)
		assert.NotEmpty(t, strings.TrimSpace(out), "the nodegroup worker must be listed")
	})

	t.Run("TolerateAllDaemonSetRunsOnWorkersOnly", func(t *testing.T) {
		const ds = "cpiso-tolerate-all"
		manifest := filepath.Join(artifacts, "cpiso-ds.yaml")
		require.NoError(t, os.WriteFile(manifest, []byte(tolerateAllDaemonSet(ds)), 0o600))
		out, err := kc.Run(60*time.Second, "apply", "-f", manifest)
		require.NoErrorf(t, err, "apply daemonset:\n%s", out)
		t.Cleanup(func() {
			_, _ = kc.Run(60*time.Second, "delete", "daemonset", ds, "-n", "default", "--ignore-not-found")
		})

		harness.EventuallyErr(t, func() error {
			want, err := kc.Run(30*time.Second, "get", "daemonset", ds, "-o", "jsonpath={.status.desiredNumberScheduled}")
			if err != nil {
				return fmt.Errorf("get daemonset: %v\n%s", err, want)
			}
			nodes, err := kc.Run(30*time.Second, "get", "nodes", "-o", "name")
			if err != nil {
				return fmt.Errorf("get nodes: %v\n%s", err, nodes)
			}
			got := len(strings.Fields(nodes))
			if strings.TrimSpace(want) != fmt.Sprint(got) {
				return fmt.Errorf("daemonset desires %q pods, cluster has %d Nodes", strings.TrimSpace(want), got)
			}
			return nil
		}, 3*time.Minute, 5*time.Second)
	})

	t.Run("MetricsServerReady", func(t *testing.T) {
		harness.EventuallyErr(t, func() error {
			out, err := kc.Run(30*time.Second, "get", "deployment", "metrics-server", "-n", "kube-system",
				"-o", "jsonpath={.status.readyReplicas}")
			if err != nil {
				return fmt.Errorf("get metrics-server: %v\n%s", err, out)
			}
			if strings.TrimSpace(out) == "" || strings.TrimSpace(out) == "0" {
				return fmt.Errorf("metrics-server has no ready replica (%q)", strings.TrimSpace(out))
			}
			return nil
		}, 5*time.Minute, 10*time.Second)

		harness.EventuallyErr(t, func() error {
			out, err := kc.Run(60*time.Second, "top", "nodes")
			if err != nil {
				return fmt.Errorf("kubectl top nodes: %v\n%s", err, out)
			}
			return nil
		}, 3*time.Minute, 10*time.Second)
	})

	t.Run("LogsWorkForEveryKubeSystemPod", func(t *testing.T) {
		out, err := kc.Run(30*time.Second, "get", "pods", "-n", "kube-system",
			"-o", "jsonpath={range .items[*]}{.metadata.name}{\"\\n\"}{end}")
		require.NoErrorf(t, err, "list kube-system pods:\n%s", out)
		pods := strings.Fields(out)
		require.NotEmpty(t, pods, "kube-system must have pods")
		for _, pod := range pods {
			logs, err := kc.Run(60*time.Second, "logs", pod, "-n", "kube-system", "--all-containers", "--tail=1")
			assert.NoErrorf(t, err, "kubectl logs %s must work:\n%s", pod, logs)
		}
	})
}

func tolerateAllDaemonSet(name string) string {
	return fmt.Sprintf(`apiVersion: apps/v1
kind: DaemonSet
metadata:
  name: %[1]s
  namespace: default
spec:
  selector:
    matchLabels:
      app: %[1]s
  template:
    metadata:
      labels:
        app: %[1]s
    spec:
      tolerations:
      - operator: Exists
      containers:
      - name: pause
        image: public.ecr.aws/docker/library/busybox:latest
        command: ["sleep", "3600"]
`, name)
}
