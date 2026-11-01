//go:build e2e

package eks

import (
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/elbv2"
	"github.com/aws/aws-sdk-go/service/iam"
	"github.com/mulgadc/spinifex/tests/e2e/harness"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	lbcChartRepo    = "https://aws.github.io/eks-charts"
	lbcChartVersion = "1.11.0"
	lbcRelease      = "aws-load-balancer-controller"

	// lbcPolicy is deliberately broad: the subject is the chart and the
	// webhook wiring, not the controller's least-privilege policy.
	lbcPolicy = `{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Action":["elasticloadbalancing:*","ec2:*","acm:*"],"Resource":"*"}]}`
)

// runLBCChart installs the unmodified upstream aws-load-balancer-controller
// chart with the values an AWS user passes (cluster name, region, VPC, the
// ServiceAccount role annotation) and reconciles an Ingress into an active ALB.
// Beyond those it needs three Spinifex-specific values:
//   - awsApiEndpoints: the gateway serves EC2, ELBv2, ACM and STS on one address
//     instead of per-service regional endpoints.
//   - enableShield, enableWaf, enableWafv2 = false: the gateway has no Shield or
//     WAF APIs, and the controller defaults to calling them.
//   - the ALBSingleSubnet feature gate: the e2e VPC has one subnet, where AWS
//     requires two availability zones.
//
// The controller gets its credentials, endpoint override and CA trust from the
// pod-identity webhook; no chart value or manifest carries them.
func runLBCChart(t *testing.T, c *harness.AWSClient, env *harness.Env, artifacts string, fx *clusterFixture) {
	helm, err := exec.LookPath(envOr("HELM_BIN", "helm"))
	if err != nil {
		t.Skipf("helm not found on PATH (%v); skipping the LBC chart subtest", err)
	}
	require.NotNil(t, fx.Cluster.Identity, "ACTIVE cluster must expose Identity")
	require.NotNil(t, fx.Cluster.Identity.Oidc, "ACTIVE cluster must expose Identity.Oidc")
	u, err := url.Parse(aws.StringValue(fx.Cluster.Identity.Oidc.Issuer))
	require.NoError(t, err, "parse issuer URL")
	gatewayURL := u.Scheme + "://" + u.Host
	region := envOr("SPINIFEX_AWS_REGION", "ap-southeast-2")

	_, roleArn, roleName := registerOIDCRole(t, c, fx, "lbc", "E2E LBC chart role")
	polOut, err := c.IAM.CreatePolicy(&iam.CreatePolicyInput{
		PolicyName:     aws.String(roleName + "-policy"),
		PolicyDocument: aws.String(lbcPolicy),
	})
	require.NoError(t, err, "create-policy")
	policyArn := aws.StringValue(polOut.Policy.Arn)
	t.Cleanup(func() {
		_, _ = c.IAM.DeletePolicy(&iam.DeletePolicyInput{PolicyArn: aws.String(policyArn)})
	})
	_, err = c.IAM.AttachRolePolicy(&iam.AttachRolePolicyInput{
		RoleName: aws.String(roleName), PolicyArn: aws.String(policyArn),
	})
	require.NoError(t, err, "attach-role-policy")
	t.Cleanup(func() {
		_, _ = c.IAM.DetachRolePolicy(&iam.DetachRolePolicyInput{
			RoleName: aws.String(roleName), PolicyArn: aws.String(policyArn),
		})
	})

	nodeRoleArn, _ := ensureNodeRole(t, c, fx)
	createWorkerNodegroup(t, c, fx, "lbc-e2e-ng", nodeRoleArn, nil)

	kcPath := writeKubeconfig(t, artifacts, fx.Cluster)
	kc := harness.NewKubectl(t, kcPath, getTokenEnv(t, env))

	runHelm := func(timeout time.Duration, args ...string) (string, error) {
		cmd := exec.Command(helm, append([]string{"--kubeconfig", kcPath}, args...)...)
		cmd.Env = append(os.Environ(), kc.AWSEnv...)
		type result struct {
			out []byte
			err error
		}
		done := make(chan result, 1)
		go func() {
			out, rerr := cmd.CombinedOutput()
			done <- result{out, rerr}
		}()
		select {
		case r := <-done:
			return string(r.out), r.err
		case <-time.After(timeout):
			if cmd.Process != nil {
				_ = cmd.Process.Kill()
			}
			return "", fmt.Errorf("helm %s timed out after %s", args[0], timeout)
		}
	}

	harness.OnFailure(t, func() {
		for name, args := range map[string][]string{
			"lbc-pods.txt":    {"-n", "kube-system", "get", "pods", "-o", "wide"},
			"lbc-logs.txt":    {"-n", "kube-system", "logs", "-l", "app.kubernetes.io/name=" + lbcRelease, "--tail=200"},
			"lbc-ingress.txt": {"-n", "default", "describe", "ingress", "lbc-e2e"},
			"lbc-events.txt":  {"get", "events", "-A", "--sort-by", ".lastTimestamp"},
		} {
			out, _ := kc.Run(45*time.Second, args...)
			harness.DumpFile(t, artifacts, name, []byte(out))
		}
	})

	harness.Phase(t, "Installing %s chart %s", lbcRelease, lbcChartVersion)
	out, err := runHelm(3*time.Minute, "repo", "add", "eks", lbcChartRepo, "--force-update")
	require.NoErrorf(t, err, "helm repo add:\n%s", out)
	t.Cleanup(func() {
		_, _ = runHelm(3*time.Minute, "uninstall", lbcRelease, "-n", "kube-system", "--wait")
	})
	out, err = runHelm(10*time.Minute, "install", lbcRelease, "eks/"+lbcRelease,
		"--version", lbcChartVersion, "-n", "kube-system", "--wait", "--timeout", "8m",
		// What an AWS user passes.
		"--set", "clusterName="+fx.ClusterName,
		"--set", "region="+region,
		"--set", "vpcId="+fx.VPCID,
		"--set", "serviceAccount.name="+lbcRelease,
		"--set-string", `serviceAccount.annotations.eks\.amazonaws\.com/role-arn=`+roleArn,
		// What Spinifex adds.
		"--set", "awsApiEndpoints=EC2="+gatewayURL+"\\,Elastic Load Balancing v2="+gatewayURL+"\\,ACM="+gatewayURL+"\\,STS="+gatewayURL,
		"--set", "enableShield=false",
		"--set", "enableWaf=false",
		"--set", "enableWafv2=false",
		"--set", "controllerConfig.featureGates.ALBSingleSubnet=true",
	)
	require.NoErrorf(t, err, "helm install:\n%s", out)

	// The webhook, not the chart, wired the controller to the gateway.
	injected, err := kc.Run(30*time.Second, "-n", "kube-system", "get", "pods",
		"-l", "app.kubernetes.io/name="+lbcRelease, "-o",
		`jsonpath={range .items[0].spec.containers[0].env[*]}{.name}{"\n"}{end}`)
	require.NoErrorf(t, err, "read controller env:\n%s", injected)
	for _, want := range []string{"AWS_ROLE_ARN", "AWS_WEB_IDENTITY_TOKEN_FILE", "AWS_CA_BUNDLE", "SSL_CERT_FILE"} {
		assert.Contains(t, injected, want, "controller pod must carry the injected %s", want)
	}

	manifest := fmt.Sprintf(`apiVersion: apps/v1
kind: Deployment
metadata:
  name: lbc-e2e
  namespace: default
spec:
  replicas: 1
  selector:
    matchLabels: {app: lbc-e2e}
  template:
    metadata:
      labels: {app: lbc-e2e}
    spec:
      nodeSelector:
        eks.amazonaws.com/nodegroup: lbc-e2e-ng
      containers:
      - name: web
        image: public.ecr.aws/docker/library/nginx:latest
        ports: [{containerPort: 80}]
---
apiVersion: v1
kind: Service
metadata:
  name: lbc-e2e
  namespace: default
spec:
  selector: {app: lbc-e2e}
  ports: [{port: 80, targetPort: 80}]
---
apiVersion: networking.k8s.io/v1
kind: Ingress
metadata:
  name: lbc-e2e
  namespace: default
  annotations:
    alb.ingress.kubernetes.io/scheme: internal
    alb.ingress.kubernetes.io/target-type: ip
    alb.ingress.kubernetes.io/subnets: %s
spec:
  ingressClassName: alb
  defaultBackend:
    service:
      name: lbc-e2e
      port: {number: 80}
`, fx.SubnetID)
	path := filepath.Join(artifacts, "lbc-ingress.yaml")
	require.NoError(t, os.WriteFile(path, []byte(manifest), 0o600))
	t.Cleanup(func() {
		_, _ = kc.Run(2*time.Minute, "delete", "-f", path, "--ignore-not-found", "--wait=true")
	})
	out, err = kc.Run(60*time.Second, "apply", "-f", path)
	require.NoErrorf(t, err, "apply ingress:\n%s", out)

	var dns string
	harness.EventuallyErr(t, func() error {
		got, gerr := kc.Run(30*time.Second, "-n", "default", "get", "ingress", "lbc-e2e", "-o",
			"jsonpath={.status.loadBalancer.ingress[0].hostname}")
		if gerr != nil {
			return fmt.Errorf("get ingress: %v\n%s", gerr, got)
		}
		if strings.TrimSpace(got) == "" {
			return fmt.Errorf("ingress has no load balancer address yet")
		}
		dns = strings.TrimSpace(got)
		return nil
	}, 8*time.Minute, 10*time.Second)

	harness.EventuallyErr(t, func() error {
		lbs, lerr := c.ELBv2.DescribeLoadBalancers(&elbv2.DescribeLoadBalancersInput{})
		if lerr != nil {
			return fmt.Errorf("describe-load-balancers: %w", lerr)
		}
		for _, lb := range lbs.LoadBalancers {
			if aws.StringValue(lb.DNSName) != dns {
				continue
			}
			if st := aws.StringValue(lb.State.Code); st != elbv2.LoadBalancerStateEnumActive {
				return fmt.Errorf("ALB %s state %q, want active", dns, st)
			}
			assert.Equal(t, elbv2.LoadBalancerTypeEnumApplication, aws.StringValue(lb.Type))
			return nil
		}
		return fmt.Errorf("no ALB with DNS name %s", dns)
	}, 5*time.Minute, 10*time.Second)
}
