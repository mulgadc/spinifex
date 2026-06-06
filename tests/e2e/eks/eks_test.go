//go:build e2e

package eks

import (
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/awserr"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/aws/aws-sdk-go/service/eks"
	"github.com/aws/aws-sdk-go/service/sts"
	"github.com/mulgadc/spinifex/tests/e2e/harness"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	eksVPCCIDR    = "10.210.0.0/16"
	eksSubnetCIDR = "10.210.1.0/24"
	eksClusterPfx = "eks-e2e"
	getTokenTpl   = "k8s-aws-v1."
)

// TestEKS drives the EKS control-plane lifecycle against the local awsgw
// endpoint: a real VPC/subnet, CreateCluster → ACTIVE (which boots the K3s
// server VM + NLB), kubeconfig artifact assembly, AccessEntry CRUD, get-token,
// and DeleteCluster → gone. Phase 1 is API-level only (no kubectl); the
// kubectl/IRSA data-plane subtests land in Phase 2 once apiserver reachability
// from the runner is wired.
//
// One cluster fixture is shared across the subtests (create once, delete in
// Cleanup) — control-plane boot is the slowest step, so re-creating per subtest
// would blow the suite timeout on dev nodes.
func TestEKS(t *testing.T) {
	env := harness.LoadEnv(t)
	artifacts := harness.ArtifactDir(t, env)
	c := harness.NewAWSClient(t, env)

	fx := setupClusterFixture(t, c, env, artifacts)

	t.Run("CreateCluster", func(t *testing.T) {
		cl := harness.WaitForEKSClusterActive(t, c, fx.ClusterName)
		assert.Equal(t, eks.ClusterStatusActive, aws.StringValue(cl.Status))
		assert.NotEmpty(t, aws.StringValue(cl.Endpoint), "ACTIVE cluster must expose an endpoint")
		require.NotNil(t, cl.CertificateAuthority, "ACTIVE cluster must expose certificateAuthority")
		assert.NotEmpty(t, aws.StringValue(cl.CertificateAuthority.Data), "certificateAuthority.data must be populated")
		ca, err := base64.StdEncoding.DecodeString(aws.StringValue(cl.CertificateAuthority.Data))
		require.NoError(t, err, "certificateAuthority.data must be base64")
		assert.Contains(t, string(ca), "BEGIN CERTIFICATE", "CA data must be a PEM cert")
		fx.Cluster = cl
	})

	t.Run("DescribeKubeconfigArtifacts", func(t *testing.T) {
		requireClusterReady(t, fx)
		path := writeKubeconfig(t, artifacts, fx.Cluster)
		raw, err := os.ReadFile(path) //nolint:gosec // artifact path built by the test
		require.NoError(t, err)
		kc := string(raw)
		assert.Contains(t, kc, "server: "+aws.StringValue(fx.Cluster.Endpoint), "kubeconfig server = cluster endpoint")
		assert.Contains(t, kc, "certificate-authority-data:", "kubeconfig embeds the CA")
		assert.Contains(t, kc, "command: aws", "kubeconfig exec block shells aws")
		assert.Contains(t, kc, "get-token", "kubeconfig exec block calls eks get-token")
		assert.Contains(t, kc, fx.ClusterName, "kubeconfig exec block targets this cluster")
	})

	t.Run("AccessEntry", func(t *testing.T) {
		requireClusterReady(t, fx)
		// CreateCluster already seeds a system:masters AccessEntry for the caller
		// principal (BootstrapClusterCreatorAdminPermissions), so exercise the API
		// against a *distinct* principal to avoid ResourceInUse on the bootstrap one.
		principal := fmt.Sprintf("arn:aws:iam::%s:role/%s-extra", fx.AccountID, fx.ClusterName)
		_, err := c.EKS.CreateAccessEntry(&eks.CreateAccessEntryInput{
			ClusterName:      aws.String(fx.ClusterName),
			PrincipalArn:     aws.String(principal),
			KubernetesGroups: aws.StringSlice([]string{"system:masters"}),
			Username:         aws.String("e2e-extra-admin"),
		})
		require.NoError(t, err, "create-access-entry")
		t.Cleanup(func() {
			_, _ = c.EKS.DeleteAccessEntry(&eks.DeleteAccessEntryInput{
				ClusterName:  aws.String(fx.ClusterName),
				PrincipalArn: aws.String(principal),
			})
		})

		list, err := c.EKS.ListAccessEntries(&eks.ListAccessEntriesInput{ClusterName: aws.String(fx.ClusterName)})
		require.NoError(t, err, "list-access-entries")
		assert.Contains(t, aws.StringValueSlice(list.AccessEntries), principal, "new entry must appear in list")

		desc, err := c.EKS.DescribeAccessEntry(&eks.DescribeAccessEntryInput{
			ClusterName:  aws.String(fx.ClusterName),
			PrincipalArn: aws.String(principal),
		})
		require.NoError(t, err, "describe-access-entry")
		require.NotNil(t, desc.AccessEntry)
		assert.Equal(t, principal, aws.StringValue(desc.AccessEntry.PrincipalArn))
		assert.Equal(t, "e2e-extra-admin", aws.StringValue(desc.AccessEntry.Username))
		assert.Contains(t, aws.StringValueSlice(desc.AccessEntry.KubernetesGroups), "system:masters")
	})

	t.Run("GetToken", func(t *testing.T) {
		requireClusterReady(t, fx)
		token := awsEKSGetToken(t, env, fx.ClusterName)
		require.True(t, strings.HasPrefix(token, getTokenTpl), "token must carry the %q prefix, got %.16q", getTokenTpl, token)

		raw, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(token, getTokenTpl))
		require.NoError(t, err, "token body must be base64url(presigned URL)")
		url := string(raw)
		assert.Contains(t, url, "Action=GetCallerIdentity", "presigned URL must call GetCallerIdentity")
		assert.Contains(t, url, "X-Amz-Signature=", "presigned URL must be signed")
		assert.Contains(t, strings.ToLower(url), "x-k8s-aws-id", "presigned URL must pin the cluster via x-k8s-aws-id")
	})

	t.Run("KubectlGetNodes", func(t *testing.T) {
		requireClusterReady(t, fx)
		// Reachability shim: the kubeconfig endpoint (NLB DNS) is unresolvable
		// from here, so point kubectl at the control-plane VM's mgmt IP:6443
		// directly. The serving cert SANs the NLB DNS + node IPs, not the mgmt
		// IP, so skip TLS verification — auth still flows through the get-token
		// webhook, which is what this asserts.
		mgmtIP := harness.ControlPlaneMgmtIP(t, env, fx.AccountID, fx.ClusterName)
		kcPath := writeKubectlKubeconfig(t, artifacts, fx.Cluster, mgmtIP)
		kc := harness.NewKubectl(t, kcPath, getTokenEnv(t, env))

		// Auth + reachability: poll until the apiserver answers and reports a
		// Ready node. A 401 here means the get-token webhook chain regressed.
		harness.EventuallyErr(t, func() error {
			out, err := kc.Run(30*time.Second, "get", "nodes",
				"-o", `jsonpath={range .items[*]}{.metadata.name}{"="}{.status.conditions[?(@.type=="Ready")].status}{"\n"}{end}`)
			if err != nil {
				return fmt.Errorf("kubectl get nodes: %v\n%s", err, out)
			}
			if !strings.Contains(out, "=True") {
				return fmt.Errorf("no Ready node yet:\n%s", out)
			}
			return nil
		}, 3*time.Minute, 5*time.Second)

		out, _ := kc.Run(30*time.Second, "get", "nodes", "-o", "wide")
		t.Logf("kubectl get nodes:\n%s", out)
	})

	t.Run("DeleteCluster", func(t *testing.T) {
		requireClusterReady(t, fx)
		_, err := c.EKS.DeleteCluster(&eks.DeleteClusterInput{Name: aws.String(fx.ClusterName)})
		require.NoError(t, err, "delete-cluster")
		harness.WaitForEKSClusterDeleted(t, c, fx.ClusterName)
		fx.Deleted = true
	})
}

// --- Fixture --------------------------------------------------------------

type clusterFixture struct {
	ClusterName string
	AccountID   string
	VPCID       string
	SubnetID    string
	Cluster     *eks.Cluster
	Deleted     bool
}

func setupClusterFixture(t *testing.T, c *harness.AWSClient, env *harness.Env, artifacts string) *clusterFixture {
	t.Helper()
	fx := &clusterFixture{ClusterName: fmt.Sprintf("%s-%d", eksClusterPfx, time.Now().Unix())}

	harness.Phase(t, "Resolving caller account")
	ident, err := c.STS.GetCallerIdentity(&sts.GetCallerIdentityInput{})
	require.NoError(t, err, "sts get-caller-identity")
	fx.AccountID = aws.StringValue(ident.Account)
	t.Logf("account: %s", fx.AccountID)

	harness.Phase(t, "Creating VPC topology (%s)", eksVPCCIDR)
	createVPC(t, c, fx)
	t.Cleanup(func() { deleteVPC(t, c, fx) })
	createSubnet(t, c, fx)
	t.Cleanup(func() { deleteSubnet(t, c, fx) })

	harness.Phase(t, "Creating cluster %q", fx.ClusterName)
	roleArn := fmt.Sprintf("arn:aws:iam::%s:role/%s-role", fx.AccountID, fx.ClusterName)
	_, err = c.EKS.CreateCluster(&eks.CreateClusterInput{
		Name:    aws.String(fx.ClusterName),
		RoleArn: aws.String(roleArn),
		ResourcesVpcConfig: &eks.VpcConfigRequest{
			SubnetIds: aws.StringSlice([]string{fx.SubnetID}),
		},
	})
	require.NoError(t, err, "create-cluster")
	t.Cleanup(func() { deleteClusterBestEffort(t, c, fx) })

	harness.OnFailure(t, func() { dumpCluster(t, c, artifacts, fx.ClusterName) })
	return fx
}

func requireClusterReady(t *testing.T, fx *clusterFixture) {
	t.Helper()
	if fx.Cluster == nil {
		t.Skip("cluster never reached ACTIVE (CreateCluster subtest failed)")
	}
}

// --- VPC / Subnet ---------------------------------------------------------

func createVPC(t *testing.T, c *harness.AWSClient, fx *clusterFixture) {
	t.Helper()
	out, err := c.EC2.CreateVpc(&ec2.CreateVpcInput{CidrBlock: aws.String(eksVPCCIDR)})
	require.NoError(t, err, "create-vpc")
	fx.VPCID = aws.StringValue(out.Vpc.VpcId)
	t.Logf("VPC: %s", fx.VPCID)
}

func deleteVPC(t *testing.T, c *harness.AWSClient, fx *clusterFixture) {
	if fx.VPCID == "" {
		return
	}
	if _, err := c.EC2.DeleteVpc(&ec2.DeleteVpcInput{VpcId: aws.String(fx.VPCID)}); err != nil {
		t.Logf("delete VPC %s: %v", fx.VPCID, err)
	}
}

func createSubnet(t *testing.T, c *harness.AWSClient, fx *clusterFixture) {
	t.Helper()
	out, err := c.EC2.CreateSubnet(&ec2.CreateSubnetInput{
		VpcId:     aws.String(fx.VPCID),
		CidrBlock: aws.String(eksSubnetCIDR),
	})
	require.NoError(t, err, "create-subnet")
	fx.SubnetID = aws.StringValue(out.Subnet.SubnetId)
	t.Logf("subnet: %s", fx.SubnetID)
}

func deleteSubnet(t *testing.T, c *harness.AWSClient, fx *clusterFixture) {
	if fx.SubnetID == "" {
		return
	}
	if _, err := c.EC2.DeleteSubnet(&ec2.DeleteSubnetInput{SubnetId: aws.String(fx.SubnetID)}); err != nil {
		t.Logf("delete subnet %s: %v", fx.SubnetID, err)
	}
}

// deleteClusterBestEffort tears the cluster down if a subtest left it standing
// (the DeleteCluster subtest already removed it on the happy path). VPC/subnet
// cleanup must wait for the cluster's NLB + VM to release them, so this runs
// before the VPC/subnet Cleanups (Cleanups run LIFO; this is registered last).
func deleteClusterBestEffort(t *testing.T, c *harness.AWSClient, fx *clusterFixture) {
	if fx.Deleted {
		return
	}
	if _, err := c.EKS.DeleteCluster(&eks.DeleteClusterInput{Name: aws.String(fx.ClusterName)}); err != nil {
		var aerr awserr.Error
		if errors.As(err, &aerr) && aerr.Code() == eks.ErrCodeResourceNotFoundException {
			return
		}
		t.Logf("cleanup delete-cluster %s: %v", fx.ClusterName, err)
		return
	}
	harness.WaitForEKSClusterDeleted(t, c, fx.ClusterName)
}

// --- kubeconfig artifact --------------------------------------------------

// writeKubeconfig assembles the kubeconfig `aws eks update-kubeconfig` would
// produce from DescribeCluster output and writes it to the artifact dir. The
// suite asserts the structure (server, CA, exec block) rather than shelling out
// to update-kubeconfig so the assertion is hermetic.
func writeKubeconfig(t *testing.T, artifacts string, cl *eks.Cluster) string {
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

// writeKubectlKubeconfig assembles a kubeconfig pointed at the control-plane
// VM's reachable mgmt IP (Phase-2 shim) with TLS verification disabled, and an
// exec block that mints a bearer token via `aws eks get-token`. Distinct from
// writeKubeconfig (the Phase-1 artifact built from the real NLB endpoint).
func writeKubectlKubeconfig(t *testing.T, artifacts string, cl *eks.Cluster, mgmtIP string) string {
	t.Helper()
	name := aws.StringValue(cl.Name)
	server := fmt.Sprintf("https://%s:6443", mgmtIP)
	kc := fmt.Sprintf(`apiVersion: v1
kind: Config
clusters:
- name: %[1]s
  cluster:
    server: %[2]s
    insecure-skip-tls-verify: true
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
`, name, server)

	path := filepath.Join(artifacts, "kubectl-"+name+".yaml")
	require.NoError(t, os.WriteFile(path, []byte(kc), 0o600), "write kubectl kubeconfig")
	t.Logf("kubectl kubeconfig: %s (server=%s)", path, server)
	return path
}

// --- get-token ------------------------------------------------------------

// awsEKSGetToken shells the stock AWS CLI `aws eks get-token`, which presigns an
// STS GetCallerIdentity URL client-side. Inherits the caller env (AWS_PROFILE,
// endpoint, CA bundle) so it signs against the same awsgw the SDK clients use.
func awsEKSGetToken(t *testing.T, env *harness.Env, cluster string) string {
	t.Helper()
	cmd := exec.Command("aws", "eks", "get-token", "--cluster-name", cluster, "--output", "text")
	cmd.Env = getTokenEnv(t, env)
	out, err := cmd.CombinedOutput()
	require.NoErrorf(t, err, "aws eks get-token:\n%s", out)
	// `--output text` prints the token in the last whitespace-separated field.
	fields := strings.Fields(string(out))
	require.NotEmpty(t, fields, "get-token produced no output")
	return fields[len(fields)-1]
}

func getTokenEnv(t *testing.T, env *harness.Env) []string {
	t.Helper()
	e := os.Environ()
	if os.Getenv("AWS_PROFILE") == "" {
		e = append(e, "AWS_PROFILE="+envOr("SPINIFEX_AWS_PROFILE", "spinifex"))
	}
	if os.Getenv("AWS_REGION") == "" {
		e = append(e, "AWS_REGION="+envOr("SPINIFEX_AWS_REGION", "ap-southeast-2"))
	}
	// Trust the spinifex CA for the presign STS call unless the profile already
	// carries a ca_bundle / the run opted into insecure mode.
	if os.Getenv("AWS_CA_BUNDLE") == "" && os.Getenv("SPINIFEX_AWS_INSECURE") != "1" {
		if ca, err := harness.ResolveCACert(env); err == nil {
			e = append(e, "AWS_CA_BUNDLE="+ca)
		}
	}
	return e
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// --- diagnostics ----------------------------------------------------------

func dumpCluster(t *testing.T, c *harness.AWSClient, artifacts, name string) {
	out, err := c.EKS.DescribeCluster(&eks.DescribeClusterInput{Name: aws.String(name)})
	if err != nil {
		t.Logf("dumpCluster describe %s: %v", name, err)
		return
	}
	path := filepath.Join(artifacts, "cluster-"+name+".txt")
	_ = os.WriteFile(path, []byte(out.Cluster.String()), 0o600)
	t.Logf("cluster dump: %s (status=%s)", path, aws.StringValue(out.Cluster.Status))
}
