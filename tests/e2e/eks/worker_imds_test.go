//go:build e2e

package eks

import (
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/aws/aws-sdk-go/service/eks"
	"github.com/mulgadc/spinifex/tests/e2e/harness"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// createWorkerNodegroup creates a one-worker nodegroup (optionally from a launch
// template), waits for it to be ACTIVE, and deletes it on cleanup.
func createWorkerNodegroup(t *testing.T, c *harness.AWSClient, fx *clusterFixture, nodegroup, nodeRoleArn string, lt *eks.LaunchTemplateSpecification) {
	t.Helper()
	harness.Phase(t, "Creating worker nodegroup %s", nodegroup)
	_, err := c.EKS.CreateNodegroup(&eks.CreateNodegroupInput{
		ClusterName:    aws.String(fx.ClusterName),
		NodegroupName:  aws.String(nodegroup),
		Subnets:        aws.StringSlice([]string{fx.SubnetID}),
		NodeRole:       aws.String(nodeRoleArn),
		LaunchTemplate: lt,
		ScalingConfig: &eks.NodegroupScalingConfig{
			MinSize:     aws.Int64(1),
			MaxSize:     aws.Int64(1),
			DesiredSize: aws.Int64(1),
		},
	})
	require.NoError(t, err, "create-nodegroup")
	t.Cleanup(func() { harness.DeleteEKSNodegroupAndWait(t, c, fx.ClusterName, nodegroup) })
	harness.EventuallyErr(t, func() error {
		out, derr := c.EKS.DescribeNodegroup(&eks.DescribeNodegroupInput{
			ClusterName:   aws.String(fx.ClusterName),
			NodegroupName: aws.String(nodegroup),
		})
		if derr != nil {
			return fmt.Errorf("describe-nodegroup: %w", derr)
		}
		if s := aws.StringValue(out.Nodegroup.Status); s != eks.NodegroupStatusActive {
			return fmt.Errorf("nodegroup status %q, want ACTIVE", s)
		}
		return nil
	}, 8*time.Minute, 10*time.Second)
}

// workerMetadataOptions returns the IMDS options DescribeInstances reports for
// the nodegroup's single running worker.
func workerMetadataOptions(t *testing.T, c *harness.AWSClient, fx *clusterFixture, nodegroup string) *ec2.InstanceMetadataOptionsResponse {
	t.Helper()
	var opts *ec2.InstanceMetadataOptionsResponse
	harness.EventuallyErr(t, func() error {
		out, err := c.EC2.DescribeInstances(&ec2.DescribeInstancesInput{Filters: []*ec2.Filter{
			{Name: aws.String("tag:spinifex:eks-cluster"), Values: aws.StringSlice([]string{fx.ClusterName})},
			{Name: aws.String("tag:spinifex:eks-nodegroup"), Values: aws.StringSlice([]string{nodegroup})},
			{Name: aws.String("instance-state-name"), Values: aws.StringSlice([]string{"running"})},
		}})
		if err != nil {
			return fmt.Errorf("describe-instances: %w", err)
		}
		var workers []*ec2.Instance
		for _, r := range out.Reservations {
			workers = append(workers, r.Instances...)
		}
		if len(workers) != 1 {
			return fmt.Errorf("want 1 running worker in nodegroup %s, got %d", nodegroup, len(workers))
		}
		if workers[0].MetadataOptions == nil {
			return fmt.Errorf("worker %s reports no MetadataOptions", aws.StringValue(workers[0].InstanceId))
		}
		opts = workers[0].MetadataOptions
		return nil
	}, 2*time.Minute, 5*time.Second)
	return opts
}

// runWorkerIMDS proves the managed-nodegroup IMDS posture end to end: a worker
// launched without a launch template reports IMDSv2 required and hop limit 2,
// and a pod with no IRSA gets the node role's credentials from IMDS through the
// SDK default chain, which a hop limit of 1 would block.
func runWorkerIMDS(t *testing.T, c *harness.AWSClient, env *harness.Env, artifacts string, fx *clusterFixture) {
	require.NotNil(t, fx.Cluster.Identity, "ACTIVE cluster must expose Identity")
	require.NotNil(t, fx.Cluster.Identity.Oidc, "ACTIVE cluster must expose Identity.Oidc")
	u, err := url.Parse(aws.StringValue(fx.Cluster.Identity.Oidc.Issuer))
	require.NoError(t, err, "parse issuer URL")
	require.NotEmpty(t, u.Host, "issuer URL must carry a host")
	gatewayBaseURL := u.Scheme + "://" + u.Host
	region := envOr("SPINIFEX_AWS_REGION", "ap-southeast-2")

	// The node role carries no permission policy: GetCallerIdentity needs none.
	nodeRoleArn, nodeRoleName := ensureNodeRole(t, c, fx)

	const nodegroup = "imds-e2e-ng"
	createWorkerNodegroup(t, c, fx, nodegroup, nodeRoleArn, nil)

	opts := workerMetadataOptions(t, c, fx, nodegroup)
	assert.Equal(t, int64(2), aws.Int64Value(opts.HttpPutResponseHopLimit), "managed nodegroup worker hop limit")
	assert.Equal(t, ec2.HttpTokensStateRequired, aws.StringValue(opts.HttpTokens), "managed nodegroup worker tokens")

	kcPath := writeKubeconfig(t, artifacts, fx.Cluster)
	kc := harness.NewKubectl(t, kcPath, getTokenEnv(t, env))

	caPath, err := harness.ResolveCACert(env)
	require.NoError(t, err, "resolve gateway CA cert")
	const caConfigMap = "imds-pod-gateway-ca"
	out, err := kc.Run(30*time.Second, "create", "configmap", caConfigMap,
		"-n", "default", "--from-file=ca.pem="+caPath)
	require.NoErrorf(t, err, "create gateway-ca configmap:\n%s", out)
	t.Cleanup(func() {
		_, _ = kc.Run(30*time.Second, "delete", "configmap", caConfigMap, "-n", "default", "--ignore-not-found")
	})

	const podName = "imds-pod-e2e"
	manifestPath := filepath.Join(artifacts, "imds-pod.yaml")
	require.NoError(t, os.WriteFile(manifestPath,
		[]byte(imdsPodManifest(podName, gatewayBaseURL, region, caConfigMap, nodegroup)), 0o600))
	t.Cleanup(func() {
		_, _ = kc.Run(60*time.Second, "delete", "-f", manifestPath, "--ignore-not-found", "--wait=false")
	})
	harness.OnFailure(t, func() {
		dumps := map[string][]string{
			"imds-pod-describe.txt": {"-n", "default", "describe", "pod", podName},
			"imds-pod-events.txt":   {"-n", "default", "get", "events", "--sort-by", ".lastTimestamp"},
		}
		for name, args := range dumps {
			dump, _ := kc.Run(45*time.Second, args...)
			harness.DumpFile(t, artifacts, name, []byte(dump))
		}
		errOut, _ := kc.Run(30*time.Second, "-n", "default", "exec", podName, "--", "cat", "/tmp/err.txt")
		harness.DumpFile(t, artifacts, "imds-pod-err.txt", []byte(errOut))
	})

	harness.Phase(t, "Applying the no-IRSA pod")
	out, err = kc.Run(60*time.Second, "apply", "-f", manifestPath)
	require.NoErrorf(t, err, "apply imds pod:\n%s", out)
	waitPodReady(t, kc, podName)

	var ident struct {
		Account string `json:"Account"`
		Arn     string `json:"Arn"`
	}
	harness.EventuallyErr(t, func() error {
		raw, err := kc.Run(30*time.Second, "-n", "default", "exec", podName, "--", "cat", "/tmp/out.json")
		if err != nil {
			return fmt.Errorf("exec cat out.json: %v\n%s", err, raw)
		}
		if jerr := json.Unmarshal([]byte(raw), &ident); jerr != nil {
			return fmt.Errorf("parse out.json: %v\nraw: %s", jerr, raw)
		}
		return nil
	}, 3*time.Minute, 5*time.Second)

	t.Logf("in-cluster GetCallerIdentity via IMDS: %+v", ident)
	assert.Equal(t, fx.AccountID, ident.Account, "caller account must match")
	assert.Contains(t, ident.Arn, "assumed-role/"+nodeRoleName,
		"a pod without IRSA must resolve to the node role through IMDS")
}

// imdsPodManifest renders a pod on the default ServiceAccount with no IRSA
// wiring: only the STS endpoint and gateway CA are set, so the SDK default chain
// falls through to the instance's IMDS role credentials.
func imdsPodManifest(pod, gatewayBaseURL, region, caConfigMap, nodegroup string) string {
	return fmt.Sprintf(`apiVersion: v1
kind: Pod
metadata:
  name: %[1]s
  namespace: default
spec:
  restartPolicy: Never
  nodeSelector:
    eks.amazonaws.com/nodegroup: %[5]s
  containers:
  - name: aws-cli
    image: public.ecr.aws/aws-cli/aws-cli:latest
    command: ["sh", "-c", "aws sts get-caller-identity --output json >/tmp/out.json 2>/tmp/err.txt; sleep 3600"]
    env:
    - name: AWS_REGION
      value: %[3]q
    - name: AWS_DEFAULT_REGION
      value: %[3]q
    - name: AWS_ENDPOINT_URL_STS
      value: %[2]q
    - name: AWS_CA_BUNDLE
      value: /etc/spinifex/gateway-ca/ca.pem
    - name: SSL_CERT_FILE
      value: /etc/spinifex/gateway-ca/ca.pem
    volumeMounts:
    - name: gateway-ca
      mountPath: /etc/spinifex/gateway-ca
      readOnly: true
  volumes:
  - name: gateway-ca
    configMap:
      name: %[4]s
`, pod, gatewayBaseURL, region, caConfigMap, nodegroup)
}

// runWorkerIMDSLaunchTemplate proves a nodegroup created from a launch template
// launches its worker with the template's metadata options, not the managed
// default: hop limit 3 differs from the default of 2.
func runWorkerIMDSLaunchTemplate(t *testing.T, c *harness.AWSClient, fx *clusterFixture) {
	nodeRoleArn, _ := ensureNodeRole(t, c, fx)

	ltName := fx.ClusterName + "-imds-lt"
	ltOut, err := c.EC2.CreateLaunchTemplate(&ec2.CreateLaunchTemplateInput{
		LaunchTemplateName: aws.String(ltName),
		LaunchTemplateData: &ec2.RequestLaunchTemplateData{
			MetadataOptions: &ec2.LaunchTemplateInstanceMetadataOptionsRequest{
				HttpTokens:              aws.String(ec2.LaunchTemplateHttpTokensStateRequired),
				HttpPutResponseHopLimit: aws.Int64(3),
			},
		},
	})
	require.NoError(t, err, "create-launch-template")
	ltID := aws.StringValue(ltOut.LaunchTemplate.LaunchTemplateId)
	t.Cleanup(func() {
		_, _ = c.EC2.DeleteLaunchTemplate(&ec2.DeleteLaunchTemplateInput{LaunchTemplateId: aws.String(ltID)})
	})

	const nodegroup = "imds-lt-e2e-ng"
	createWorkerNodegroup(t, c, fx, nodegroup, nodeRoleArn, &eks.LaunchTemplateSpecification{
		Id:      aws.String(ltID),
		Version: aws.String("1"),
	})

	opts := workerMetadataOptions(t, c, fx, nodegroup)
	assert.Equal(t, int64(3), aws.Int64Value(opts.HttpPutResponseHopLimit), "worker must take the template hop limit")
	assert.Equal(t, ec2.HttpTokensStateRequired, aws.StringValue(opts.HttpTokens), "worker tokens")
}
