//go:build e2e

package eks

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/eks"
	"github.com/mulgadc/spinifex/tests/e2e/harness"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const cpIsolationNodegroup = "cpiso-e2e-ng"

// nodeGroupLabelRows prints one "<node> <nodegroup>" row per Node; the second
// column is empty for a Node that is not a nodegroup worker.
const nodeGroupLabelRows = `jsonpath={range .items[*]}{.metadata.name} {.metadata.labels.eks\.amazonaws\.com/nodegroup}{"\n"}{end}`

// nodeReadyRows prints one "<node>=<Ready status>" row per Node.
const nodeReadyRows = `jsonpath={range .items[*]}{.metadata.name}{"="}{.status.conditions[?(@.type=="Ready")].status}{"\n"}{end}`

// runCPIsolation proves control-plane hosts never register as Nodes, as on AWS:
// only nodegroup workers are listed and one becomes Ready, a DaemonSet tolerating
// every taint runs on workers only, metrics-server and its APIService become
// available, and logs work for every kube-system pod on a Ready node.
func runCPIsolation(t *testing.T, c *harness.AWSClient, env *harness.Env, artifacts string, fx *clusterFixture) {
	nodeRoleArn, _ := ensureNodeRole(t, c, fx)
	createWorkerNodegroup(t, c, fx, cpIsolationNodegroup, nodeRoleArn, nil)

	kcPath := writeKubeconfig(t, artifacts, fx.Cluster)
	kc := harness.NewKubectl(t, kcPath, getTokenEnv(t, env))

	t.Run("WorkerNodeReady", func(t *testing.T) {
		harness.EventuallyErr(t, func() error {
			out, err := kc.Run(30*time.Second, "get", "nodes", "-o", nodeReadyRows)
			if err != nil {
				return fmt.Errorf("kubectl get nodes: %v\n%s", err, out)
			}
			if !strings.Contains(out, "=True") {
				return fmt.Errorf("no Ready node yet:\n%s", out)
			}
			return nil
		}, 6*time.Minute, 5*time.Second)
	})

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
		}, 10*time.Minute, 10*time.Second)

		// The aggregated metrics API must be Available: apiserver discovery dials
		// it through the konnectivity tunnel, and while it is not, namespace GC
		// never clears the kubernetes finalizer.
		harness.EventuallyErr(t, func() error {
			out, err := kc.Run(30*time.Second, "get", "apiservice", "v1beta1.metrics.k8s.io",
				"-o", `jsonpath={.status.conditions[?(@.type=="Available")].status}`)
			if err != nil {
				return fmt.Errorf("get apiservice v1beta1.metrics.k8s.io: %v\n%s", err, out)
			}
			if strings.TrimSpace(out) != "True" {
				return fmt.Errorf("v1beta1.metrics.k8s.io Available=%q", strings.TrimSpace(out))
			}
			return nil
		}, 3*time.Minute, 10*time.Second)

		harness.EventuallyErr(t, func() error {
			out, err := kc.Run(60*time.Second, "top", "nodes")
			if err != nil {
				return fmt.Errorf("kubectl top nodes: %v\n%s", err, out)
			}
			return nil
		}, 3*time.Minute, 10*time.Second)
	})

	t.Run("AddonDeletion", func(t *testing.T) {
		// Namespace GC needs every aggregated API available, so addon deletion
		// is checked once a worker exists.
		if _, err := c.EKS.DescribeAddon(&eks.DescribeAddonInput{
			ClusterName: aws.String(fx.ClusterName),
			AddonName:   aws.String(noopAddon),
		}); err != nil {
			t.Skipf("addon %s was not created: %v", noopAddon, err)
		}
		_, err := c.EKS.DeleteAddon(&eks.DeleteAddonInput{
			ClusterName: aws.String(fx.ClusterName),
			AddonName:   aws.String(noopAddon),
		})
		require.NoError(t, err, "delete-addon")
		harness.EventuallyErr(t, func() error {
			out, runErr := kc.Run(30*time.Second, "get", "namespace", noopAddon,
				"--ignore-not-found", "-o", `jsonpath={.metadata.name}`)
			if runErr != nil {
				return fmt.Errorf("kubectl get namespace: %v\n%s", runErr, out)
			}
			if strings.TrimSpace(out) != "" {
				return fmt.Errorf("namespace still present")
			}
			return nil
		}, 3*time.Minute, 10*time.Second)
	})

	t.Run("LogsWorkForEveryKubeSystemPod", func(t *testing.T) {
		nodes, err := kc.Run(30*time.Second, "get", "nodes", "-o", nodeReadyRows)
		require.NoErrorf(t, err, "get nodes:\n%s", nodes)
		ready := map[string]bool{}
		for _, row := range strings.Fields(nodes) {
			if name, status, ok := strings.Cut(row, "="); ok && status == "True" {
				ready[name] = true
			}
		}
		out, err := kc.Run(30*time.Second, "get", "pods", "-n", "kube-system",
			"-o", `jsonpath={range .items[*]}{.metadata.name} {.spec.nodeName}{"\n"}{end}`)
		require.NoErrorf(t, err, "list kube-system pods:\n%s", out)
		checked := 0
		for _, row := range strings.Split(strings.TrimSpace(out), "\n") {
			f := strings.Fields(row)
			if len(f) != 2 || !ready[f[1]] {
				continue
			}
			checked++
			logs, err := kc.Run(60*time.Second, "logs", f[0], "-n", "kube-system", "--all-containers", "--tail=1")
			assert.NoErrorf(t, err, "kubectl logs %s must work:\n%s", f[0], logs)
		}
		require.NotZero(t, checked, "kube-system must have pods on a Ready node")
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
