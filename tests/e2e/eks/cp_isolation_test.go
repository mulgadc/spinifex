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

// nodeGroupLabelRows prints one "<node> <nodegroup>" row per Node; the second
// column is empty for a Node that is not a nodegroup worker.
const nodeGroupLabelRows = `jsonpath={range .items[*]}{.metadata.name} {.metadata.labels.eks\.amazonaws\.com/nodegroup}{"\n"}{end}`

// runCPIsolation proves control-plane hosts never register as Nodes, as on AWS:
// only nodegroup workers are listed, a DaemonSet tolerating every taint runs on
// workers only, metrics-server is Ready, and logs work for every kube-system pod.
func runCPIsolation(t *testing.T, c *harness.AWSClient, env *harness.Env, artifacts string, fx *clusterFixture) {
	nodeRoleArn, _ := ensureNodeRole(t, c, fx)
	createWorkerNodegroup(t, c, fx, cpIsolationNodegroup, nodeRoleArn, nil)

	kcPath := writeKubeconfig(t, artifacts, fx.Cluster)
	kc := harness.NewKubectl(t, kcPath, getTokenEnv(t, env))

	t.Run("OnlyWorkersAreNodes", func(t *testing.T) {
		out, err := kc.Run(30*time.Second, "get", "nodes", "-o", nodeGroupLabelRows)
		require.NoErrorf(t, err, "get nodes:\n%s", out)
		rows := strings.Split(strings.TrimSpace(out), "\n")
		require.NotEmpty(t, strings.TrimSpace(out), "the nodegroup worker must be listed")
		for _, row := range rows {
			f := strings.Fields(row)
			assert.Lenf(t, f, 2, "Node %q must carry the eks.amazonaws.com/nodegroup label: a Node without it is not a nodegroup worker", row)
		}
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
			ready, err := kc.Run(30*time.Second, "get", "pods", "-l", "app="+ds,
				"-o", `jsonpath={range .items[*]}{.status.conditions[?(@.type=="Ready")].status}{"\n"}{end}`)
			if err != nil {
				return fmt.Errorf("get daemonset pods: %v\n%s", err, ready)
			}
			states := strings.Fields(ready)
			if len(states) == 0 {
				return fmt.Errorf("daemonset has no pods yet")
			}
			for _, s := range states {
				if s != "True" {
					return fmt.Errorf("daemonset pods not all Ready: %v", states)
				}
			}
			return nil
		}, 3*time.Minute, 5*time.Second)

		nodes, err := kc.Run(30*time.Second, "get", "pods", "-l", "app="+ds,
			"-o", `jsonpath={range .items[*]}{.spec.nodeName}{"\n"}{end}`)
		require.NoErrorf(t, err, "get daemonset pod nodes:\n%s", nodes)
		for _, node := range strings.Fields(nodes) {
			label, err := kc.Run(30*time.Second, "get", "node", node, "-o",
				`jsonpath={.metadata.labels.eks\.amazonaws\.com/nodegroup}`)
			require.NoErrorf(t, err, "get node %s:\n%s", node, label)
			assert.NotEmptyf(t, strings.TrimSpace(label), "daemonset pod ran on %s, which is not a nodegroup worker", node)
		}
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
