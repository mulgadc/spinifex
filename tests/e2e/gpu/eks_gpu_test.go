//go:build e2e

package gpu

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/awserr"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/aws/aws-sdk-go/service/eks"
	"github.com/aws/aws-sdk-go/service/iam"
	"github.com/aws/aws-sdk-go/service/sts"
	"github.com/mulgadc/spinifex/tests/e2e/harness"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	// eksGPUInstanceType is the single worker instance type for the GPU
	// nodegroup — must be advertised with GpuInfo (see requireEKSGPUFixture).
	eksGPUInstanceType = "g5.xlarge"
	// eksGPUVPCCIDR / eksGPUSubnetCIDR / eksGPUPublicSubnetCIDR build a
	// dedicated VPC + private worker subnet + NAT-gateway egress topology
	// (mirrors eks_test.go's setupClusterFixture), on a CIDR range disjoint
	// from the eks suite's 10.210.0.0/16 so the two suites never collide.
	eksGPUVPCCIDR          = "10.212.0.0/16"
	eksGPUSubnetCIDR       = "10.212.1.0/24"
	eksGPUPublicSubnetCIDR = "10.212.2.0/24"
	eksGPUClusterPfx       = "eks-gpu-e2e"

	// eksNodeAMIName / eksNodeGPUAMIName are the registered EKS node AMI
	// names (see spinifex/spinifex/utils/images.go). Both must be imported —
	// the base image for control-plane parity checks, the GPU variant for
	// resolveWorkerAMI's GPU branch.
	eksNodeAMIName    = "spinifex-eks-node"
	eksNodeGPUAMIName = "spinifex-eks-node-gpu"

	// nvidiaDevicePluginDaemonSet is the DaemonSet name from the bundled
	// addon manifest (scripts/images/eks-node/addons/nvidia-device-plugin).
	nvidiaDevicePluginDaemonSet = "nvidia-device-plugin-daemonset"

	// nvidiaSmiPodImage carries the nvidia-smi CLI; pulled directly (not
	// through the ECR mirror path), matching the busybox pattern EBS CSI e2e
	// uses for its non-ECR test images.
	nvidiaSmiPodImage = "nvidia/cuda:12.4.1-base-ubuntu22.04"
)

// eksGPUFixture bundles the environment + AWS client resolved once every
// requireEKSGPUFixture guard condition passes.
type eksGPUFixture struct {
	Env *harness.Env
	AWS *harness.AWSClient
}

// requireEKSGPUFixture skips the calling test unless every precondition for
// exercising Epic B (EKS: expose GPU to pods) is met:
//   - SPINIFEX_E2E is set
//   - SPINIFEX_MODE is single (this suite only targets a single-node dev box)
//   - g5.xlarge is advertised with GPU capability by DescribeInstanceTypes
//   - both EKS node AMIs (spinifex-eks-node, spinifex-eks-node-gpu) are imported
func requireEKSGPUFixture(t *testing.T) *eksGPUFixture {
	t.Helper()
	if os.Getenv("SPINIFEX_E2E") == "" {
		t.Skip("SPINIFEX_E2E unset")
	}
	env := harness.LoadEnv(t)
	if env.Mode != harness.ModeSingle {
		t.Skip("eks gpu suite requires SPINIFEX_MODE=single")
	}
	// Give a clean skip rather than letting NewAWSClient t.Fatal when no
	// Spinifex node is running (ResolveCACert uses the same candidate paths).
	if _, err := harness.ResolveCACert(env); err != nil {
		t.Skip("no Spinifex node running — provision first: ansible-playbook ansible/playbooks/dev-reset.yml")
	}
	awsCli := harness.NewAWSClient(t, env)

	if reason := discoverEKSGPUPrereqs(awsCli); reason != "" {
		t.Skip(reason)
	}
	return &eksGPUFixture{Env: env, AWS: awsCli}
}

// discoverEKSGPUPrereqs returns a non-empty skip reason unless g5.xlarge is
// advertised with GPU capability AND both EKS node AMIs are imported.
func discoverEKSGPUPrereqs(c *harness.AWSClient) string {
	typesOut, err := c.EC2.DescribeInstanceTypes(&ec2.DescribeInstanceTypesInput{
		InstanceTypes: aws.StringSlice([]string{eksGPUInstanceType}),
	})
	if err != nil {
		return "DescribeInstanceTypes: " + err.Error()
	}
	gpuAdvertised := false
	for _, it := range typesOut.InstanceTypes {
		if aws.StringValue(it.InstanceType) == eksGPUInstanceType && it.GpuInfo != nil && len(it.GpuInfo.Gpus) > 0 {
			gpuAdvertised = true
			break
		}
	}
	if !gpuAdvertised {
		return fmt.Sprintf("instance type %s not advertised with GPU capability — node has no GPU or gpu_passthrough is disabled", eksGPUInstanceType)
	}

	imgsOut, err := c.EC2.DescribeImages(&ec2.DescribeImagesInput{
		Filters: []*ec2.Filter{
			{Name: aws.String("name"), Values: aws.StringSlice([]string{eksNodeAMIName, eksNodeGPUAMIName})},
			{Name: aws.String("state"), Values: []*string{aws.String("available")}},
		},
	})
	if err != nil {
		return "DescribeImages: " + err.Error()
	}
	seen := make(map[string]bool, len(imgsOut.Images))
	for _, img := range imgsOut.Images {
		seen[aws.StringValue(img.Name)] = true
	}
	var missing []string
	for _, name := range []string{eksNodeAMIName, eksNodeGPUAMIName} {
		if !seen[name] {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		return fmt.Sprintf("EKS node AMI(s) not imported: %s — run: spx admin images import --name <ami>", strings.Join(missing, ", "))
	}
	return ""
}

// TestEKSGPUPodExposure drives Epic B end-to-end: a minimal EKS cluster plus a
// 1-node GPU nodegroup, then asserts the GPU is actually visible to a pod —
// node Ready, gpu.present label + scheduling taint, device-plugin DaemonSet
// rollout, nvidia.com/gpu allocatable, and a tolerating pod running
// nvidia-smi against the passed-through device.
func TestEKSGPUPodExposure(t *testing.T) {
	fx := requireEKSGPUFixture(t)
	env, c := fx.Env, fx.AWS
	artifacts := harness.ArtifactDir(t, env)

	harness.Phase(t, "EKS GPU — resolving caller account")
	ident, err := c.STS.GetCallerIdentity(&sts.GetCallerIdentityInput{})
	require.NoError(t, err, "sts get-caller-identity")
	accountID := aws.StringValue(ident.Account)

	harness.Phase(t, "Creating VPC topology (%s)", eksGPUVPCCIDR)
	vpcID := harness.CreateVPC(t, c, eksGPUVPCCIDR)
	t.Cleanup(func() { harness.DeleteVPC(t, c, vpcID) })
	subnetID := harness.CreateSubnet(t, c, vpcID, eksGPUSubnetCIDR)
	t.Cleanup(func() { harness.DeleteSubnet(t, c, subnetID) })
	egress := harness.CreateWorkerEgress(t, c, vpcID, subnetID, eksGPUPublicSubnetCIDR)
	t.Cleanup(func() { harness.DeleteWorkerEgress(t, c, egress) })

	clusterName := fmt.Sprintf("%s-%d", eksGPUClusterPfx, time.Now().Unix())
	harness.Phase(t, "Creating cluster %q", clusterName)
	roleArn := fmt.Sprintf("arn:aws:iam::%s:role/%s-role", accountID, clusterName)
	// e2e:allow-create — the GPU cluster is the subject under test (GPU worker join + device-plugin exposure).
	_, err = c.EKS.CreateCluster(&eks.CreateClusterInput{
		Name:    aws.String(clusterName),
		RoleArn: aws.String(roleArn),
		ResourcesVpcConfig: &eks.VpcConfigRequest{
			SubnetIds: aws.StringSlice([]string{subnetID}),
			// Public access (default). The dedicated VPC has a NAT Gateway
			// (harness.CreateWorkerEgress), so the private GPU worker SNATs
			// out to reach the internet-facing NLB endpoint and pull images.
			EndpointPublicAccess: aws.Bool(true),
		},
	})
	require.NoError(t, err, "create-cluster")
	t.Cleanup(func() { deleteEKSGPUClusterBestEffort(t, c, clusterName) })

	cl := harness.WaitForEKSClusterActive(t, c, clusterName)
	require.Equal(t, eks.ClusterStatusActive, aws.StringValue(cl.Status))

	kcPath := writeEKSGPUKubeconfig(t, artifacts, cl)
	kc := harness.NewKubectl(t, kcPath, eksGPUTokenEnv(t, env))

	nodeRoleName := fmt.Sprintf("%s-node", clusterName)
	harness.Phase(t, "Creating node IAM role %s", nodeRoleName)
	const nodeTrustPolicy = `{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Principal":{"Service":"ec2.amazonaws.com"},"Action":"sts:AssumeRole"}]}`
	_, err = c.IAM.CreateRole(&iam.CreateRoleInput{
		RoleName:                 aws.String(nodeRoleName),
		AssumeRolePolicyDocument: aws.String(nodeTrustPolicy),
		Description:              aws.String("E2E GPU node role"),
	})
	require.NoError(t, err, "create-role (node)")
	t.Cleanup(func() { deleteEKSGPUNodeRoleBestEffort(t, c, nodeRoleName) })

	const nodegroup = "gpu-e2e-ng"
	harness.Phase(t, "Creating GPU nodegroup %s (%s)", nodegroup, eksGPUInstanceType)
	// e2e:allow-create — the GPU nodegroup is the subject under test (GPU worker join + device-plugin exposure).
	_, err = c.EKS.CreateNodegroup(&eks.CreateNodegroupInput{
		ClusterName:   aws.String(clusterName),
		NodegroupName: aws.String(nodegroup),
		Subnets:       aws.StringSlice([]string{subnetID}),
		NodeRole:      aws.String(fmt.Sprintf("arn:aws:iam::%s:role/%s", accountID, nodeRoleName)),
		InstanceTypes: aws.StringSlice([]string{eksGPUInstanceType}),
		ScalingConfig: &eks.NodegroupScalingConfig{
			MinSize:     aws.Int64(1),
			MaxSize:     aws.Int64(1),
			DesiredSize: aws.Int64(1),
		},
	})
	require.NoError(t, err, "create-nodegroup")
	t.Cleanup(func() { deleteEKSGPUNodegroupBestEffort(t, c, clusterName, nodegroup) })

	harness.Phase(t, "Waiting for nodegroup ACTIVE")
	harness.EventuallyErr(t, func() error {
		out, derr := c.EKS.DescribeNodegroup(&eks.DescribeNodegroupInput{
			ClusterName:   aws.String(clusterName),
			NodegroupName: aws.String(nodegroup),
		})
		if derr != nil {
			return fmt.Errorf("describe-nodegroup: %w", derr)
		}
		if s := aws.StringValue(out.Nodegroup.Status); s != eks.NodegroupStatusActive {
			return fmt.Errorf("nodegroup status %q, want ACTIVE", s)
		}
		return nil
	}, 10*time.Minute, 10*time.Second)

	// (a) the GPU worker node becomes Ready.
	var nodeName string
	harness.Phase(t, "Waiting for GPU worker node Ready")
	harness.EventuallyErr(t, func() error {
		out, kerr := kc.Run(30*time.Second, "get", "nodes",
			"-l", "eks.amazonaws.com/nodegroup="+nodegroup,
			"-o", `jsonpath={range .items[*]}{.metadata.name}{"="}{.status.conditions[?(@.type=="Ready")].status}{"\n"}{end}`)
		if kerr != nil {
			return fmt.Errorf("kubectl get nodes: %v\n%s", kerr, out)
		}
		for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
			name, ready, ok := strings.Cut(line, "=")
			if ok && ready == "True" {
				nodeName = name
				return nil
			}
		}
		return fmt.Errorf("no Ready GPU node yet:\n%s", out)
	}, 8*time.Minute, 5*time.Second)
	require.NotEmpty(t, nodeName, "GPU worker node name resolved")
	harness.Detail(t, "node", nodeName)

	// (b) node carries the GPU label and scheduling taint.
	labelOut, err := kc.Run(30*time.Second, "get", "node", nodeName, "-o",
		`jsonpath={.metadata.labels['nvidia\.com/gpu\.present']}`)
	require.NoErrorf(t, err, "get node labels:\n%s", labelOut)
	assert.Equal(t, "true", strings.TrimSpace(labelOut), "node must carry nvidia.com/gpu.present=true")

	taintsOut, err := kc.Run(30*time.Second, "get", "node", nodeName, "-o",
		`jsonpath={range .spec.taints[*]}{.key}={.value}:{.effect}{"\n"}{end}`)
	require.NoErrorf(t, err, "get node taints:\n%s", taintsOut)
	assert.Contains(t, taintsOut, "nvidia.com/gpu=present:NoSchedule", "node must carry the GPU scheduling taint")

	// (c) nvidia-device-plugin-daemonset reports desired=1/ready=1.
	harness.Phase(t, "Waiting for %s rollout", nvidiaDevicePluginDaemonSet)
	harness.EventuallyErr(t, func() error {
		out, kerr := kc.Run(30*time.Second, "-n", "kube-system", "get", "daemonset", nvidiaDevicePluginDaemonSet,
			"-o", `jsonpath={.status.desiredNumberScheduled}={.status.numberReady}`)
		if kerr != nil {
			return fmt.Errorf("get daemonset: %v\n%s", kerr, out)
		}
		if strings.TrimSpace(out) != "1=1" {
			return fmt.Errorf("daemonset desired=ready %q, want 1=1", strings.TrimSpace(out))
		}
		return nil
	}, 5*time.Minute, 5*time.Second)

	// (d) node.status.allocatable["nvidia.com/gpu"] == "1".
	allocOut, err := kc.Run(30*time.Second, "get", "node", nodeName, "-o",
		`jsonpath={.status.allocatable['nvidia\.com/gpu']}`)
	require.NoErrorf(t, err, "get node allocatable:\n%s", allocOut)
	assert.Equal(t, "1", strings.TrimSpace(allocOut), "node must expose exactly 1 allocatable nvidia.com/gpu")

	// (e) a pod tolerating the taint and requesting nvidia.com/gpu runs
	// nvidia-smi and observes the passed-through device.
	const podName = "nvidia-smi-e2e"
	podPath := filepath.Join(artifacts, "nvidia-smi-pod.yaml")
	require.NoError(t, os.WriteFile(podPath, []byte(nvidiaSmiPodManifest(podName)), 0o600))
	t.Cleanup(func() {
		_, _ = kc.Run(60*time.Second, "delete", "-f", podPath, "--ignore-not-found", "--wait=false")
	})

	harness.Phase(t, "Applying nvidia-smi pod")
	out, err := kc.Run(60*time.Second, "apply", "-f", podPath)
	require.NoErrorf(t, err, "apply nvidia-smi pod:\n%s", out)

	harness.EventuallyErr(t, func() error {
		phase, kerr := kc.Run(30*time.Second, "get", "pod", podName, "-o", `jsonpath={.status.phase}`)
		if kerr != nil {
			return fmt.Errorf("get pod phase: %v\n%s", kerr, phase)
		}
		switch strings.TrimSpace(phase) {
		case "Succeeded":
			return nil
		case "Failed":
			desc, _ := kc.Run(30*time.Second, "describe", "pod", podName)
			return fmt.Errorf("pod %s failed:\n%s", podName, desc)
		default:
			return fmt.Errorf("pod %s phase=%q, want Succeeded", podName, strings.TrimSpace(phase))
		}
	}, 5*time.Minute, 5*time.Second)

	logs, err := kc.Run(30*time.Second, "logs", podName)
	require.NoErrorf(t, err, "pod logs:\n%s", logs)
	assert.Contains(t, logs, "NVIDIA RTX A1000", "nvidia-smi output must report the passed-through GPU model")
}

// nvidiaSmiPodManifest renders a Never-restart pod that tolerates the GPU
// scheduling taint, requests one nvidia.com/gpu, and runs nvidia-smi once.
func nvidiaSmiPodManifest(name string) string {
	return fmt.Sprintf(`apiVersion: v1
kind: Pod
metadata:
  name: %s
  namespace: default
spec:
  restartPolicy: Never
  tolerations:
  - key: nvidia.com/gpu
    operator: Equal
    value: present
    effect: NoSchedule
  containers:
  - name: nvidia-smi
    image: %s
    command: ["nvidia-smi"]
    resources:
      limits:
        nvidia.com/gpu: "1"
`, name, nvidiaSmiPodImage)
}

// writeEKSGPUKubeconfig builds a kubeconfig from DescribeCluster output,
// mirroring eks_test.go's writeKubeconfig (unexported there, so reimplemented
// locally for this package).
func writeEKSGPUKubeconfig(t *testing.T, artifacts string, cl *eks.Cluster) string {
	t.Helper()
	name := aws.StringValue(cl.Name)
	kc := fmt.Sprintf(`apiVersion: v1
kind: Config
clusters:
- name: %[1]s
  cluster:
    server: %[2]s
    certificate-authority-data: %[3]s
contexts:
- name: %[1]s
  context:
    cluster: %[1]s
    user: %[1]s
current-context: %[1]s
users:
- name: %[1]s
  user:
    exec:
      apiVersion: client.authentication.k8s.io/v1beta1
      command: aws
      args:
      - eks
      - get-token
      - --cluster-name
      - %[1]s
`, name, aws.StringValue(cl.Endpoint), aws.StringValue(cl.CertificateAuthority.Data))

	path := filepath.Join(artifacts, "kubeconfig-"+name+".yaml")
	require.NoError(t, os.WriteFile(path, []byte(kc), 0o600), "write kubeconfig")
	t.Logf("kubeconfig: %s", path)
	return path
}

// eksGPUTokenEnv builds the environment kubectl's `aws eks get-token` exec
// block needs, mirroring eks_test.go's getTokenEnv.
func eksGPUTokenEnv(t *testing.T, env *harness.Env) []string {
	t.Helper()
	e := os.Environ()
	if os.Getenv("AWS_PROFILE") == "" {
		e = append(e, "AWS_PROFILE="+eksGPUEnvOr("SPINIFEX_AWS_PROFILE", "spinifex"))
	}
	if os.Getenv("AWS_REGION") == "" {
		e = append(e, "AWS_REGION="+eksGPUEnvOr("SPINIFEX_AWS_REGION", "ap-southeast-2"))
	}
	if os.Getenv("AWS_CA_BUNDLE") == "" && os.Getenv("SPINIFEX_AWS_INSECURE") != "1" {
		if ca, err := harness.ResolveCACert(env); err == nil {
			e = append(e, "AWS_CA_BUNDLE="+ca)
		}
	}
	return e
}

func eksGPUEnvOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// deleteEKSGPUNodegroupBestEffort deletes the nodegroup and waits for it to
// clear, logging (not failing) on error so cluster teardown still proceeds.
func deleteEKSGPUNodegroupBestEffort(t *testing.T, c *harness.AWSClient, cluster, nodegroup string) {
	t.Helper()
	if _, err := c.EKS.DeleteNodegroup(&eks.DeleteNodegroupInput{
		ClusterName:   aws.String(cluster),
		NodegroupName: aws.String(nodegroup),
	}); err != nil {
		if !isEKSResourceNotFound(err) {
			t.Logf("cleanup delete-nodegroup %s/%s: %v", cluster, nodegroup, err)
		}
		return
	}
	harness.EventuallyErr(t, func() error {
		_, err := c.EKS.DescribeNodegroup(&eks.DescribeNodegroupInput{
			ClusterName:   aws.String(cluster),
			NodegroupName: aws.String(nodegroup),
		})
		if err == nil {
			return fmt.Errorf("nodegroup %s/%s still exists", cluster, nodegroup)
		}
		if isEKSResourceNotFound(err) {
			return nil
		}
		return fmt.Errorf("describe-nodegroup %s/%s: %w", cluster, nodegroup, err)
	}, 5*time.Minute, 5*time.Second)
}

// deleteEKSGPUNodeRoleBestEffort detaches the node role from the instance
// profile ensureNodeInstanceProfile created for it (see nodegroup.go), then
// deletes the profile and the role, logging (not failing) on error. Cleanups
// run LIFO, so this fires after the nodegroup cleanup has already terminated
// the worker that held the instance profile.
func deleteEKSGPUNodeRoleBestEffort(t *testing.T, c *harness.AWSClient, roleName string) {
	t.Helper()
	if _, err := c.IAM.RemoveRoleFromInstanceProfile(&iam.RemoveRoleFromInstanceProfileInput{
		InstanceProfileName: aws.String(roleName),
		RoleName:            aws.String(roleName),
	}); err != nil {
		t.Logf("cleanup remove-role-from-instance-profile %s: %v", roleName, err)
	}
	if _, err := c.IAM.DeleteInstanceProfile(&iam.DeleteInstanceProfileInput{
		InstanceProfileName: aws.String(roleName),
	}); err != nil {
		t.Logf("cleanup delete-instance-profile %s: %v", roleName, err)
	}
	if _, err := c.IAM.DeleteRole(&iam.DeleteRoleInput{RoleName: aws.String(roleName)}); err != nil {
		t.Logf("cleanup delete-role %s: %v", roleName, err)
	}
}

// deleteEKSGPUClusterBestEffort deletes the cluster and waits for it to be
// gone. Registered after the nodegroup cleanup (LIFO), so the worker is
// already reclaimed before the control plane tears down.
func deleteEKSGPUClusterBestEffort(t *testing.T, c *harness.AWSClient, cluster string) {
	t.Helper()
	if _, err := c.EKS.DeleteCluster(&eks.DeleteClusterInput{Name: aws.String(cluster)}); err != nil {
		if !isEKSResourceNotFound(err) {
			t.Logf("cleanup delete-cluster %s: %v", cluster, err)
		}
		return
	}
	harness.WaitForEKSClusterDeleted(t, c, cluster)
}

// isEKSResourceNotFound reports whether err is the EKS "gone" error. The SDK
// constant is "ResourceNotFoundException" but awsgw emits the doubled
// "ResourceNotFoundExceptionException" on the wire, so match by substring
// (mirrors harness.isClusterNotFound).
func isEKSResourceNotFound(err error) bool {
	if err == nil {
		return false
	}
	var aerr awserr.Error
	if errors.As(err, &aerr) {
		return strings.Contains(aerr.Code(), "ResourceNotFound")
	}
	return false
}
