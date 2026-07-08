//go:build e2e

package eks

import (
	"encoding/base64"
	"errors"
	"fmt"
	"net"
	"net/url"
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
	"github.com/aws/aws-sdk-go/service/iam"
	"github.com/aws/aws-sdk-go/service/sts"
	"github.com/mulgadc/spinifex/tests/e2e/harness"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	eksVPCCIDR          = "10.210.0.0/16"
	eksSubnetCIDR       = "10.210.1.0/24"
	eksPublicSubnetCIDR = "10.210.2.0/24"
	eksClusterPfx       = "eks-e2e"
	getTokenTpl         = "k8s-aws-v1."
)

// TestEKS drives the EKS control-plane lifecycle against the local awsgw
// endpoint: a customer VPC/subnet (Set A), CreateCluster → ACTIVE (spinifex
// auto-builds the managed control-plane VPC — Set B — and spreads the K3s
// control plane behind an internet-facing NLB), kubeconfig artifact assembly,
// AccessEntry CRUD, get-token, kubectl reachability against the published
// endpoint, managed-addon delivery, and DeleteCluster → gone.
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
		assertEKSEndpointResolves(t, aws.StringValue(cl.Endpoint))
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
		// Reach the apiserver at the published endpoint with TLS verification ON.
		// A 401 means the get-token webhook chain regressed; a TLS error means
		// the cert-SAN wiring regressed.
		require.NotEmpty(t, aws.StringValue(fx.Cluster.Endpoint), "cluster must publish a reachable endpoint")
		kcPath := writeKubeconfig(t, artifacts, fx.Cluster)
		kc := harness.NewKubectl(t, kcPath, getTokenEnv(t, env))

		// Poll until a Ready node appears. k3s may crash once during bootstrap
		// (etcd fsync latency under I/O contention) and be respawned; the generous
		// envelope allows the control plane to stabilise after the first ACTIVE.
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
		}, 6*time.Minute, 5*time.Second)

		out, _ := kc.Run(30*time.Second, "get", "nodes", "-o", "wide")
		t.Logf("kubectl get nodes:\n%s", out)
	})

	t.Run("AddonDelivery", func(t *testing.T) {
		requireClusterReady(t, fx)
		// End-to-end managed-addon delivery: CreateAddon stages a manifest
		// descriptor host-side; the on-VM addon-sync agent pulls it through the
		// gateway, renders the baked bundle into the K3s auto-deploy dir, and
		// reports ready, which flips the record to ACTIVE. spinifex-noop is the
		// fixture (Namespace + ConfigMap) — no dependency on the CSI/LB bundles.
		const addon = "spinifex-noop"
		_, err := c.EKS.CreateAddon(&eks.CreateAddonInput{
			ClusterName: aws.String(fx.ClusterName),
			AddonName:   aws.String(addon),
		})
		require.NoError(t, err, "create-addon")

		// Record reaches ACTIVE once the VM confirms delivery. Generous envelope:
		// one addon-sync tick (30s) + k3s auto-deploy apply, plus control-plane
		// stabilisation slack.
		harness.EventuallyErr(t, func() error {
			out, derr := c.EKS.DescribeAddon(&eks.DescribeAddonInput{
				ClusterName: aws.String(fx.ClusterName),
				AddonName:   aws.String(addon),
			})
			if derr != nil {
				return fmt.Errorf("describe-addon: %w", derr)
			}
			if s := aws.StringValue(out.Addon.Status); s != eks.AddonStatusActive {
				return fmt.Errorf("addon status %q, want ACTIVE", s)
			}
			return nil
		}, 5*time.Minute, 10*time.Second)

		// The fixture's objects must exist on the cluster.
		kcPath := writeKubeconfig(t, artifacts, fx.Cluster)
		kc := harness.NewKubectl(t, kcPath, getTokenEnv(t, env))
		out, err := kc.Run(30*time.Second, "get", "configmap", "spinifex-noop",
			"-n", "spinifex-noop", "-o", `jsonpath={.data.marker}`)
		require.NoError(t, err, "addon ConfigMap must exist: %s", out)
		assert.Contains(t, out, "delivered", "addon ConfigMap must carry the marker")

		// DeleteAddon unstages the manifest; the agent GCs the rendered file and
		// k3s' auto-deploy controller removes the objects.
		_, err = c.EKS.DeleteAddon(&eks.DeleteAddonInput{
			ClusterName: aws.String(fx.ClusterName),
			AddonName:   aws.String(addon),
		})
		require.NoError(t, err, "delete-addon")

		harness.EventuallyErr(t, func() error {
			out, runErr := kc.Run(30*time.Second, "get", "namespace", "spinifex-noop",
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

	t.Run("IRSAWebIdentity", func(t *testing.T) {
		requireClusterReady(t, fx)
		runIRSAWebIdentity(t, c, env, artifacts, fx)
	})

	t.Run("EBSCSIVolume", func(t *testing.T) {
		requireClusterReady(t, fx)
		runEBSCSIVolume(t, c, env, artifacts, fx)
	})

	t.Run("DeleteCluster", func(t *testing.T) {
		requireClusterReady(t, fx)
		_, err := c.EKS.DeleteCluster(&eks.DeleteClusterInput{Name: aws.String(fx.ClusterName)})
		require.NoError(t, err, "delete-cluster")
		harness.WaitForEKSClusterDeleted(t, c, fx.ClusterName)
		fx.Deleted = true
	})
}

// runIRSAWebIdentity exercises the IRSA token-exchange path at the API level
// (no pod, no nodegroup): register the cluster's OIDC provider, create a role
// trusting it, mint a real ServiceAccount token via `kubectl create token`
// (signed by the cluster's OIDC key, JWKS published at CreateCluster), exchange
// it through AssumeRoleWithWebIdentity, and prove the returned credentials are
// usable via GetCallerIdentity.
func runIRSAWebIdentity(t *testing.T, c *harness.AWSClient, env *harness.Env, artifacts string, fx *clusterFixture) {
	require.NotNil(t, fx.Cluster.Identity, "ACTIVE cluster must expose Identity")
	require.NotNil(t, fx.Cluster.Identity.Oidc, "ACTIVE cluster must expose Identity.Oidc")
	issuer := aws.StringValue(fx.Cluster.Identity.Oidc.Issuer)
	require.NotEmpty(t, issuer, "cluster OIDC issuer must be published")
	t.Logf("OIDC issuer: %s", issuer)

	// 1) Register the cluster's OIDC provider in the caller account so the STS
	//    handler will accept tokens carrying this issuer.
	oidcOut, err := c.IAM.CreateOpenIDConnectProvider(&iam.CreateOpenIDConnectProviderInput{
		Url:            aws.String(issuer),
		ClientIDList:   aws.StringSlice([]string{"sts.amazonaws.com"}),
		ThumbprintList: aws.StringSlice([]string{"0000000000000000000000000000000000000000"}),
	})
	require.NoError(t, err, "create-open-id-connect-provider")
	providerArn := aws.StringValue(oidcOut.OpenIDConnectProviderArn)
	require.NotEmpty(t, providerArn, "OIDC provider ARN empty")
	t.Cleanup(func() {
		_, _ = c.IAM.DeleteOpenIDConnectProvider(&iam.DeleteOpenIDConnectProviderInput{
			OpenIDConnectProviderArn: aws.String(providerArn),
		})
	})
	t.Logf("OIDC provider: %s", providerArn)

	// 2) Create a role whose trust policy federates the OIDC provider. No
	//    Condition block — the Federated principal + AssumeRoleWithWebIdentity
	//    action are sufficient to grant, which keeps the test independent of the
	//    condition-key issuer-prefix format.
	roleName := fmt.Sprintf("%s-irsa", fx.ClusterName)
	trustPolicy := fmt.Sprintf(`{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Principal":{"Federated":%q},"Action":"sts:AssumeRoleWithWebIdentity"}]}`, providerArn)
	roleOut, err := c.IAM.CreateRole(&iam.CreateRoleInput{
		RoleName:                 aws.String(roleName),
		AssumeRolePolicyDocument: aws.String(trustPolicy),
		Description:              aws.String("E2E IRSA web-identity role"),
	})
	require.NoError(t, err, "create-role")
	roleArn := aws.StringValue(roleOut.Role.Arn)
	require.NotEmpty(t, roleArn, "role ARN empty")
	t.Cleanup(func() {
		_, _ = c.IAM.DeleteRole(&iam.DeleteRoleInput{RoleName: aws.String(roleName)})
	})
	t.Logf("role: %s", roleArn)

	// 3) Mint a ServiceAccount token bound to sts.amazonaws.com. `kubectl create
	//    token` uses the TokenRequest API — no pod required. The token's iss is
	//    the cluster OIDC issuer and aud includes sts.amazonaws.com (k3s is wired
	//    with --service-account-issuer / --api-audiences at CreateCluster).
	kcPath := writeKubeconfig(t, artifacts, fx.Cluster)
	kc := harness.NewKubectl(t, kcPath, getTokenEnv(t, env))
	tokenOut, err := kc.Run(30*time.Second, "create", "token", "default",
		"--namespace", "default", "--audience", "sts.amazonaws.com")
	require.NoErrorf(t, err, "kubectl create token:\n%s", tokenOut)
	token := strings.TrimSpace(tokenOut)
	require.Equal(t, 2, strings.Count(token, "."), "web-identity token must be a JWT (3 dot-separated parts)")

	// 4) Exchange the token. AssumeRoleWithWebIdentity is anonymous (the SDK
	//    strips SigV4 for this op); the JWT is the identity.
	const sessionName = "e2e-irsa"
	var assumeOut *sts.AssumeRoleWithWebIdentityOutput
	harness.EventuallyErr(t, func() error {
		out, aerr := c.STS.AssumeRoleWithWebIdentity(&sts.AssumeRoleWithWebIdentityInput{
			RoleArn:          aws.String(roleArn),
			RoleSessionName:  aws.String(sessionName),
			WebIdentityToken: aws.String(token),
		})
		if aerr != nil {
			return fmt.Errorf("assume-role-with-web-identity: %w", aerr)
		}
		assumeOut = out
		return nil
	}, 60*time.Second, 5*time.Second)
	require.NotNil(t, assumeOut.Credentials, "AssumeRoleWithWebIdentity returned no credentials")
	assert.Equal(t, "system:serviceaccount:default:default",
		aws.StringValue(assumeOut.SubjectFromWebIdentityToken), "subject must be the default SA")
	require.True(t, strings.HasPrefix(aws.StringValue(assumeOut.Credentials.AccessKeyId), "ASIA"),
		"web-identity credentials must be temporary (ASIA…)")

	// 5) The returned temporary credentials must be usable: GetCallerIdentity
	//    must resolve to the assumed-role principal.
	sessClient := harness.NewAWSClientWithSessionCreds(t, env,
		aws.StringValue(assumeOut.Credentials.AccessKeyId),
		aws.StringValue(assumeOut.Credentials.SecretAccessKey),
		aws.StringValue(assumeOut.Credentials.SessionToken))
	ident, err := sessClient.STS.GetCallerIdentity(&sts.GetCallerIdentityInput{})
	require.NoError(t, err, "get-caller-identity with web-identity creds")
	assert.Equal(t, fx.AccountID, aws.StringValue(ident.Account), "caller account must match")
	assert.Contains(t, aws.StringValue(ident.Arn), "assumed-role/"+roleName,
		"caller ARN must reflect the assumed IRSA role")
	t.Logf("GetCallerIdentity (web-identity creds): %s", aws.StringValue(ident.Arn))
}

// runEBSCSIVolume exercises the full EBS CSI data path on the live cluster:
// install the aws-ebs-csi-driver managed addon bound to an IRSA role, then
// drive a default gp3 PVC -> Pod -> CreateVolume(Viperblock)/AttachVolume
// (virtio-blk hotplug, serial=volume-id) -> mdev by-id symlink -> ext4 mount.
// A nonce written by the first pod must survive a reschedule onto a fresh pod
// backed by the same PVC, proving the volume detaches/reattaches data-intact.
func runEBSCSIVolume(t *testing.T, c *harness.AWSClient, env *harness.Env, artifacts string, fx *clusterFixture) {
	require.NotNil(t, fx.Cluster.Identity, "ACTIVE cluster must expose Identity")
	require.NotNil(t, fx.Cluster.Identity.Oidc, "ACTIVE cluster must expose Identity.Oidc")
	issuer := aws.StringValue(fx.Cluster.Identity.Oidc.Issuer)
	require.NotEmpty(t, issuer, "cluster OIDC issuer must be published")

	// IRSA role for ebs-csi-controller-sa. awsgw does not enforce EC2 IAM
	// authorization, so the web-identity trust alone suffices (no permission
	// policy) — the controller's projected SA token is the identity it presents
	// to AssumeRoleWithWebIdentity before calling CreateVolume/AttachVolume.
	oidcOut, err := c.IAM.CreateOpenIDConnectProvider(&iam.CreateOpenIDConnectProviderInput{
		Url:            aws.String(issuer),
		ClientIDList:   aws.StringSlice([]string{"sts.amazonaws.com"}),
		ThumbprintList: aws.StringSlice([]string{"0000000000000000000000000000000000000000"}),
	})
	require.NoError(t, err, "create-open-id-connect-provider")
	providerArn := aws.StringValue(oidcOut.OpenIDConnectProviderArn)
	t.Cleanup(func() {
		_, _ = c.IAM.DeleteOpenIDConnectProvider(&iam.DeleteOpenIDConnectProviderInput{
			OpenIDConnectProviderArn: aws.String(providerArn),
		})
	})

	roleName := fmt.Sprintf("%s-ebs-csi", fx.ClusterName)
	trustPolicy := fmt.Sprintf(`{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Principal":{"Federated":%q},"Action":"sts:AssumeRoleWithWebIdentity"}]}`, providerArn)
	roleOut, err := c.IAM.CreateRole(&iam.CreateRoleInput{
		RoleName:                 aws.String(roleName),
		AssumeRolePolicyDocument: aws.String(trustPolicy),
		Description:              aws.String("E2E EBS CSI controller role"),
	})
	require.NoError(t, err, "create-role")
	roleArn := aws.StringValue(roleOut.Role.Arn)
	t.Cleanup(func() {
		_, _ = c.IAM.DeleteRole(&iam.DeleteRoleInput{RoleName: aws.String(roleName)})
	})
	t.Logf("EBS CSI IRSA role: %s", roleArn)

	// awsgw enforces IAM on assumed-role EC2 calls, so the controller's
	// CreateVolume/AttachVolume need a permission policy on the role — a
	// customer-managed one with an explicit allow (AWS-managed ARNs are opaque
	// and grant nothing). This mirrors what an operator attaches to the
	// ServiceAccountRoleArn in production.
	const ebsPolicy = `{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Action":["ec2:CreateVolume","ec2:DeleteVolume","ec2:AttachVolume","ec2:DetachVolume","ec2:ModifyVolume","ec2:DescribeVolumes","ec2:DescribeVolumesModifications","ec2:DescribeInstances","ec2:DescribeAvailabilityZones","ec2:DescribeSnapshots","ec2:CreateSnapshot","ec2:DeleteSnapshot","ec2:DescribeTags","ec2:CreateTags","ec2:DeleteTags"],"Resource":"*"}]}`
	polOut, err := c.IAM.CreatePolicy(&iam.CreatePolicyInput{
		PolicyName:     aws.String(roleName + "-policy"),
		PolicyDocument: aws.String(ebsPolicy),
	})
	require.NoError(t, err, "create-policy")
	policyArn := aws.StringValue(polOut.Policy.Arn)
	t.Cleanup(func() {
		_, _ = c.IAM.DeletePolicy(&iam.DeletePolicyInput{PolicyArn: aws.String(policyArn)})
	})
	_, err = c.IAM.AttachRolePolicy(&iam.AttachRolePolicyInput{
		RoleName:  aws.String(roleName),
		PolicyArn: aws.String(policyArn),
	})
	require.NoError(t, err, "attach-role-policy")
	t.Cleanup(func() {
		_, _ = c.IAM.DetachRolePolicy(&iam.DetachRolePolicyInput{
			RoleName:  aws.String(roleName),
			PolicyArn: aws.String(policyArn),
		})
	})

	// The EBS CSI driver obeys the control-plane taint, so it and the volume
	// workloads need a customer worker node. Create a 1-node nodegroup; its
	// worker is a customer-space instance, so the customer-owned volume can
	// attach to it. DescribeNodegroup reports ACTIVE only once the worker has
	// registered Ready.
	const nodegroup = "ebs-csi-e2e-ng"
	harness.Phase(t, "Creating worker nodegroup %s", nodegroup)
	// e2e:allow-create — the worker nodegroup is the subject under test (customer-space node for cross-space attach).
	_, err = c.EKS.CreateNodegroup(&eks.CreateNodegroupInput{
		ClusterName:   aws.String(fx.ClusterName),
		NodegroupName: aws.String(nodegroup),
		Subnets:       aws.StringSlice([]string{fx.SubnetID}),
		NodeRole:      aws.String(fmt.Sprintf("arn:aws:iam::%s:role/%s-node", fx.AccountID, fx.ClusterName)),
		ScalingConfig: &eks.NodegroupScalingConfig{
			MinSize:     aws.Int64(1),
			MaxSize:     aws.Int64(1),
			DesiredSize: aws.Int64(1),
		},
	})
	require.NoError(t, err, "create-nodegroup")
	t.Cleanup(func() {
		_, _ = c.EKS.DeleteNodegroup(&eks.DeleteNodegroupInput{
			ClusterName:   aws.String(fx.ClusterName),
			NodegroupName: aws.String(nodegroup),
		})
	})
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

	// Install the managed addon bound to the role. The addon-sync agent renders
	// the controller manifest's {{SERVICE_ACCOUNT_ROLE_ARN}} from this ARN.
	const addon = "aws-ebs-csi-driver"
	harness.Phase(t, "Installing %s addon", addon)
	_, err = c.EKS.CreateAddon(&eks.CreateAddonInput{
		ClusterName:           aws.String(fx.ClusterName),
		AddonName:             aws.String(addon),
		ServiceAccountRoleArn: aws.String(roleArn),
	})
	require.NoError(t, err, "create-addon")
	t.Cleanup(func() {
		_, _ = c.EKS.DeleteAddon(&eks.DeleteAddonInput{
			ClusterName: aws.String(fx.ClusterName),
			AddonName:   aws.String(addon),
		})
	})

	harness.EventuallyErr(t, func() error {
		out, derr := c.EKS.DescribeAddon(&eks.DescribeAddonInput{
			ClusterName: aws.String(fx.ClusterName),
			AddonName:   aws.String(addon),
		})
		if derr != nil {
			return fmt.Errorf("describe-addon: %w", derr)
		}
		if s := aws.StringValue(out.Addon.Status); s != eks.AddonStatusActive {
			return fmt.Errorf("addon status %q, want ACTIVE", s)
		}
		return nil
	}, 5*time.Minute, 10*time.Second)

	kcPath := writeKubeconfig(t, artifacts, fx.Cluster)
	kc := harness.NewKubectl(t, kcPath, getTokenEnv(t, env))

	// On failure, snapshot the CSI control path while the cluster is still up
	// (DeleteCluster runs in a later subtest). Dumps land in the artifact dir,
	// which is retained on failure.
	harness.OnFailure(t, func() {
		dumps := map[string][]string{
			"csi-describe-pvc.txt":     {"-n", "default", "describe", "pvc", "ebs-csi-e2e"},
			"csi-describe-pods.txt":    {"-n", "default", "describe", "pods"},
			"csi-get-events.txt":       {"-n", "default", "get", "events", "--sort-by", ".lastTimestamp"},
			"csi-storageclass.txt":     {"get", "storageclass", "-o", "yaml"},
			"csi-csidriver.txt":        {"get", "csidriver,csinode", "-o", "wide"},
			"csi-volumeattach.txt":     {"get", "volumeattachment", "-o", "wide"},
			"csi-ctrl-provisioner.txt": {"-n", "kube-system", "logs", "deploy/ebs-csi-controller", "-c", "csi-provisioner", "--tail", "200"},
			"csi-ctrl-plugin.txt":      {"-n", "kube-system", "logs", "deploy/ebs-csi-controller", "-c", "ebs-plugin", "--tail", "200"},
			"csi-node-plugin.txt":      {"-n", "kube-system", "logs", "daemonset/ebs-csi-node", "-c", "ebs-plugin", "--tail", "200"},
		}
		for name, args := range dumps {
			out, _ := kc.Run(45*time.Second, args...)
			harness.DumpFile(t, artifacts, name, []byte(out))
		}
	})

	// Controller Deployment + node DaemonSet must roll out before a PVC binds.
	harness.Phase(t, "Waiting for CSI driver rollout")
	harness.EventuallyErr(t, func() error {
		if out, rerr := kc.Run(60*time.Second, "-n", "kube-system", "rollout", "status",
			"deployment/ebs-csi-controller", "--timeout", "30s"); rerr != nil {
			return fmt.Errorf("controller rollout: %v\n%s", rerr, out)
		}
		if out, rerr := kc.Run(60*time.Second, "-n", "kube-system", "rollout", "status",
			"daemonset/ebs-csi-node", "--timeout", "30s"); rerr != nil {
			return fmt.Errorf("node rollout: %v\n%s", rerr, out)
		}
		return nil
	}, 3*time.Minute, 10*time.Second)

	// Provision a gp3 PVC and a pod that writes a unique marker onto the volume.
	marker := fmt.Sprintf("csi-e2e-%d", time.Now().UnixNano())
	const pvcName = "ebs-csi-e2e"
	writerPath := filepath.Join(artifacts, "ebs-csi-writer.yaml")
	require.NoError(t, os.WriteFile(writerPath, []byte(csiPVCPodManifest(pvcName, "ebs-csi-e2e-writer",
		fmt.Sprintf("echo %s > /data/marker && sync && sleep 3600", marker))), 0o600))
	t.Cleanup(func() {
		_, _ = kc.Run(60*time.Second, "delete", "-f", writerPath, "--ignore-not-found", "--wait=false")
	})

	harness.Phase(t, "Applying gp3 PVC + writer pod")
	out, err := kc.Run(60*time.Second, "apply", "-f", writerPath)
	require.NoErrorf(t, err, "apply writer:\n%s", out)

	// Bind drives CreateVolume(Viperblock) + AttachVolume(virtio-blk hotplug).
	harness.EventuallyErr(t, func() error {
		ph, perr := kc.Run(30*time.Second, "get", "pvc", pvcName, "-o", `jsonpath={.status.phase}`)
		if perr != nil {
			return fmt.Errorf("get pvc: %v\n%s", perr, ph)
		}
		if strings.TrimSpace(ph) != "Bound" {
			return fmt.Errorf("pvc phase %q, want Bound", strings.TrimSpace(ph))
		}
		return nil
	}, 5*time.Minute, 5*time.Second)
	t.Logf("PVC %s Bound", pvcName)

	waitPodReady(t, kc, "ebs-csi-e2e-writer")

	// The marker must have landed on the mounted volume.
	got, err := kc.Run(30*time.Second, "exec", "ebs-csi-e2e-writer", "--", "cat", "/data/marker")
	require.NoErrorf(t, err, "exec cat marker:\n%s", got)
	require.Equal(t, marker, strings.TrimSpace(got), "writer must observe its own marker")

	// Reschedule: delete the writer, bind the same PVC to a fresh reader pod and
	// assert the marker survived the detach/reattach + remount.
	harness.Phase(t, "Rescheduling onto a fresh pod")
	out, err = kc.Run(90*time.Second, "delete", "pod", "ebs-csi-e2e-writer", "--wait=true", "--timeout", "60s")
	require.NoErrorf(t, err, "delete writer:\n%s", out)

	readerPath := filepath.Join(artifacts, "ebs-csi-reader.yaml")
	require.NoError(t, os.WriteFile(readerPath, []byte(csiPVCPodManifest(pvcName, "ebs-csi-e2e-reader",
		"cat /data/marker && sleep 3600")), 0o600))
	t.Cleanup(func() {
		_, _ = kc.Run(60*time.Second, "delete", "-f", readerPath, "--ignore-not-found", "--wait=false")
	})
	out, err = kc.Run(60*time.Second, "apply", "-f", readerPath)
	require.NoErrorf(t, err, "apply reader:\n%s", out)
	waitPodReady(t, kc, "ebs-csi-e2e-reader")

	got, err = kc.Run(30*time.Second, "exec", "ebs-csi-e2e-reader", "--", "cat", "/data/marker")
	require.NoErrorf(t, err, "exec cat marker (reader):\n%s", got)
	assert.Equal(t, marker, strings.TrimSpace(got), "marker must survive pod reschedule")
	t.Logf("marker survived reschedule: %s", strings.TrimSpace(got))
}

// csiPVCPodManifest renders a gp3 PVC plus a single pod that mounts it at /data
// and runs cmd. Reusing the claim name lets the reader pod rebind the volume the
// writer provisioned. No control-plane toleration: the pod must land on the
// customer worker node (the volume's space), not the system control-plane.
func csiPVCPodManifest(pvc, pod, cmd string) string {
	return fmt.Sprintf(`apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: %s
  namespace: default
spec:
  accessModes: ["ReadWriteOnce"]
  storageClassName: ebs-gp3
  resources:
    requests:
      storage: 1Gi
---
apiVersion: v1
kind: Pod
metadata:
  name: %s
  namespace: default
spec:
  restartPolicy: Never
  containers:
  - name: app
    image: public.ecr.aws/docker/library/busybox:1.36
    command: ["sh", "-c", %q]
    volumeMounts:
    - name: vol
      mountPath: /data
  volumes:
  - name: vol
    persistentVolumeClaim:
      claimName: %s
`, pvc, pod, cmd, pvc)
}

// waitPodReady blocks until the named default-namespace pod reports Ready,
// dumping `describe pod` into the failure message on timeout.
func waitPodReady(t *testing.T, kc *harness.Kubectl, pod string) {
	t.Helper()
	harness.EventuallyErr(t, func() error {
		out, err := kc.Run(30*time.Second, "get", "pod", pod, "-o",
			`jsonpath={.status.conditions[?(@.type=="Ready")].status}`)
		if err != nil {
			return fmt.Errorf("get pod: %v\n%s", err, out)
		}
		if strings.TrimSpace(out) != "True" {
			desc, _ := kc.Run(30*time.Second, "describe", "pod", pod)
			return fmt.Errorf("pod %s not Ready (%q)\n%s", pod, strings.TrimSpace(out), desc)
		}
		return nil
	}, 4*time.Minute, 5*time.Second)
}

// --- Fixture --------------------------------------------------------------

type clusterFixture struct {
	ClusterName     string
	AccountID       string
	VPCID           string
	SubnetID        string // worker (private) subnet
	IGWID           string
	PubSubnetID     string
	PubRTID         string
	PubRTAssocID    string
	EIPAllocID      string
	NATGWID         string
	WorkerRTID      string
	WorkerRTAssocID string
	Cluster         *eks.Cluster
	Deleted         bool
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
	createWorkerEgress(t, c, fx)
	t.Cleanup(func() { deleteWorkerEgress(t, c, fx) })

	harness.Phase(t, "Creating cluster %q", fx.ClusterName)
	roleArn := fmt.Sprintf("arn:aws:iam::%s:role/%s-role", fx.AccountID, fx.ClusterName)
	_, err = c.EKS.CreateCluster(&eks.CreateClusterInput{
		Name:    aws.String(fx.ClusterName),
		RoleArn: aws.String(roleArn),
		ResourcesVpcConfig: &eks.VpcConfigRequest{
			SubnetIds: aws.StringSlice([]string{fx.SubnetID}),
			// Public access (default). The customer VPC has a NAT Gateway
			// (createWorkerEgress), so the private worker SNATs out to reach
			// the internet-facing NLB endpoint and pull public CSI images.
			EndpointPublicAccess: aws.Bool(true),
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

// createWorkerEgress gives the customer VPC the egress a real customer VPC
// needs for private workers: an IGW-fronted public subnet hosting a NAT
// Gateway, with the worker subnet default-routed to that NAT Gateway. AttachIGW
// deliberately programs no SNAT (mirrors AWS: an IGW serves only public-IP
// instances), so a private worker reaches the public NLB endpoint and pulls
// public CSI images only through the NAT Gateway's subnet-wide SNAT.
func createWorkerEgress(t *testing.T, c *harness.AWSClient, fx *clusterFixture) {
	t.Helper()

	igwOut, err := c.EC2.CreateInternetGateway(&ec2.CreateInternetGatewayInput{}) // e2e:allow-create
	require.NoError(t, err, "create-internet-gateway")
	fx.IGWID = aws.StringValue(igwOut.InternetGateway.InternetGatewayId)
	_, err = c.EC2.AttachInternetGateway(&ec2.AttachInternetGatewayInput{
		InternetGatewayId: aws.String(fx.IGWID),
		VpcId:             aws.String(fx.VPCID),
	})
	require.NoError(t, err, "attach-internet-gateway")

	// Public subnet routed straight to the IGW; the NAT Gateway lives here.
	pubOut, err := c.EC2.CreateSubnet(&ec2.CreateSubnetInput{ // e2e:allow-create
		VpcId:     aws.String(fx.VPCID),
		CidrBlock: aws.String(eksPublicSubnetCIDR),
	})
	require.NoError(t, err, "create-public-subnet")
	fx.PubSubnetID = aws.StringValue(pubOut.Subnet.SubnetId)

	pubRT, err := c.EC2.CreateRouteTable(&ec2.CreateRouteTableInput{VpcId: aws.String(fx.VPCID)}) // e2e:allow-create
	require.NoError(t, err, "create-public-route-table")
	fx.PubRTID = aws.StringValue(pubRT.RouteTable.RouteTableId)
	_, err = c.EC2.CreateRoute(&ec2.CreateRouteInput{
		RouteTableId:         aws.String(fx.PubRTID),
		DestinationCidrBlock: aws.String("0.0.0.0/0"),
		GatewayId:            aws.String(fx.IGWID),
	})
	require.NoError(t, err, "create-public-route")
	pubAssoc, err := c.EC2.AssociateRouteTable(&ec2.AssociateRouteTableInput{
		RouteTableId: aws.String(fx.PubRTID),
		SubnetId:     aws.String(fx.PubSubnetID),
	})
	require.NoError(t, err, "associate-public-route-table")
	fx.PubRTAssocID = aws.StringValue(pubAssoc.AssociationId)

	// Elastic IP + NAT Gateway in the public subnet.
	eip, err := c.EC2.AllocateAddress(&ec2.AllocateAddressInput{Domain: aws.String("vpc")}) // e2e:allow-create
	require.NoError(t, err, "allocate-address")
	fx.EIPAllocID = aws.StringValue(eip.AllocationId)
	natOut, err := c.EC2.CreateNatGateway(&ec2.CreateNatGatewayInput{ // e2e:allow-create
		SubnetId:     aws.String(fx.PubSubnetID),
		AllocationId: aws.String(fx.EIPAllocID),
	})
	require.NoError(t, err, "create-nat-gateway")
	fx.NATGWID = aws.StringValue(natOut.NatGateway.NatGatewayId)
	waitNatGatewayAvailable(t, c, fx.NATGWID)

	// Worker (private) subnet default-routes to the NAT Gateway; associating
	// the route table triggers the subnet-wide SNAT egress reroute.
	workerRT, err := c.EC2.CreateRouteTable(&ec2.CreateRouteTableInput{VpcId: aws.String(fx.VPCID)}) // e2e:allow-create
	require.NoError(t, err, "create-worker-route-table")
	fx.WorkerRTID = aws.StringValue(workerRT.RouteTable.RouteTableId)
	_, err = c.EC2.CreateRoute(&ec2.CreateRouteInput{
		RouteTableId:         aws.String(fx.WorkerRTID),
		DestinationCidrBlock: aws.String("0.0.0.0/0"),
		NatGatewayId:         aws.String(fx.NATGWID),
	})
	require.NoError(t, err, "create-worker-route")
	workerAssoc, err := c.EC2.AssociateRouteTable(&ec2.AssociateRouteTableInput{
		RouteTableId: aws.String(fx.WorkerRTID),
		SubnetId:     aws.String(fx.SubnetID),
	})
	require.NoError(t, err, "associate-worker-route-table")
	fx.WorkerRTAssocID = aws.StringValue(workerAssoc.AssociationId)
	t.Logf("egress: igw=%s nat=%s eip=%s pubrt=%s workerrt=%s",
		fx.IGWID, fx.NATGWID, fx.EIPAllocID, fx.PubRTID, fx.WorkerRTID)
}

// waitNatGatewayAvailable blocks until the NAT Gateway reports "available";
// SNAT is not programmed on the VPC router until then.
func waitNatGatewayAvailable(t *testing.T, c *harness.AWSClient, natID string) {
	t.Helper()
	const timeout = 3 * time.Minute
	deadline := time.Now().Add(timeout)
	for {
		out, err := c.EC2.DescribeNatGateways(&ec2.DescribeNatGatewaysInput{
			NatGatewayIds: aws.StringSlice([]string{natID}),
		})
		if err == nil && len(out.NatGateways) > 0 {
			switch aws.StringValue(out.NatGateways[0].State) {
			case "available":
				return
			case "failed", "deleted":
				t.Fatalf("nat gateway %s entered terminal state %q", natID, aws.StringValue(out.NatGateways[0].State))
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("nat gateway %s not available within %s", natID, timeout)
		}
		time.Sleep(3 * time.Second)
	}
}

// deleteWorkerEgress tears the egress topology down in reverse: the worker
// route table releases the private subnet, then the NAT Gateway is deleted and
// drained before the public subnet + EIP it pins can be removed.
func deleteWorkerEgress(t *testing.T, c *harness.AWSClient, fx *clusterFixture) {
	if fx.WorkerRTAssocID != "" {
		if _, err := c.EC2.DisassociateRouteTable(&ec2.DisassociateRouteTableInput{
			AssociationId: aws.String(fx.WorkerRTAssocID),
		}); err != nil {
			t.Logf("disassociate worker route table %s: %v", fx.WorkerRTAssocID, err)
		}
	}
	if fx.WorkerRTID != "" {
		if _, err := c.EC2.DeleteRouteTable(&ec2.DeleteRouteTableInput{
			RouteTableId: aws.String(fx.WorkerRTID),
		}); err != nil {
			t.Logf("delete worker route table %s: %v", fx.WorkerRTID, err)
		}
	}
	if fx.NATGWID != "" {
		if _, err := c.EC2.DeleteNatGateway(&ec2.DeleteNatGatewayInput{
			NatGatewayId: aws.String(fx.NATGWID),
		}); err != nil {
			t.Logf("delete nat gateway %s: %v", fx.NATGWID, err)
		}
		waitNatGatewayDeleted(t, c, fx.NATGWID)
	}
	if fx.EIPAllocID != "" {
		if _, err := c.EC2.ReleaseAddress(&ec2.ReleaseAddressInput{
			AllocationId: aws.String(fx.EIPAllocID),
		}); err != nil {
			t.Logf("release address %s: %v", fx.EIPAllocID, err)
		}
	}
	if fx.PubRTAssocID != "" {
		if _, err := c.EC2.DisassociateRouteTable(&ec2.DisassociateRouteTableInput{
			AssociationId: aws.String(fx.PubRTAssocID),
		}); err != nil {
			t.Logf("disassociate public route table %s: %v", fx.PubRTAssocID, err)
		}
	}
	if fx.PubRTID != "" {
		if _, err := c.EC2.DeleteRouteTable(&ec2.DeleteRouteTableInput{
			RouteTableId: aws.String(fx.PubRTID),
		}); err != nil {
			t.Logf("delete public route table %s: %v", fx.PubRTID, err)
		}
	}
	if fx.PubSubnetID != "" {
		if _, err := c.EC2.DeleteSubnet(&ec2.DeleteSubnetInput{SubnetId: aws.String(fx.PubSubnetID)}); err != nil {
			t.Logf("delete public subnet %s: %v", fx.PubSubnetID, err)
		}
	}
	if fx.IGWID != "" {
		if _, err := c.EC2.DetachInternetGateway(&ec2.DetachInternetGatewayInput{
			InternetGatewayId: aws.String(fx.IGWID),
			VpcId:             aws.String(fx.VPCID),
		}); err != nil {
			t.Logf("detach igw %s: %v", fx.IGWID, err)
		}
		if _, err := c.EC2.DeleteInternetGateway(&ec2.DeleteInternetGatewayInput{
			InternetGatewayId: aws.String(fx.IGWID),
		}); err != nil {
			t.Logf("delete igw %s: %v", fx.IGWID, err)
		}
	}
}

// waitNatGatewayDeleted blocks until the NAT Gateway drains to "deleted" so the
// public subnet + EIP it holds can be released; best-effort on timeout.
func waitNatGatewayDeleted(t *testing.T, c *harness.AWSClient, natID string) {
	t.Helper()
	const timeout = 3 * time.Minute
	deadline := time.Now().Add(timeout)
	for {
		out, err := c.EC2.DescribeNatGateways(&ec2.DescribeNatGatewaysInput{
			NatGatewayIds: aws.StringSlice([]string{natID}),
		})
		if err != nil || len(out.NatGateways) == 0 {
			return
		}
		if aws.StringValue(out.NatGateways[0].State) == "deleted" {
			return
		}
		if time.Now().After(deadline) {
			t.Logf("nat gateway %s not deleted within %s; continuing teardown", natID, timeout)
			return
		}
		time.Sleep(3 * time.Second)
	}
}

// deleteClusterBestEffort tears the cluster down if the DeleteCluster subtest did not.
// Registered last so it runs before VPC/subnet Cleanups (LIFO), ensuring the NLB + VM
// release the subnet before the VPC is removed.
func deleteClusterBestEffort(t *testing.T, c *harness.AWSClient, fx *clusterFixture) {
	if fx.Deleted {
		return
	}
	if _, err := c.EKS.DeleteCluster(&eks.DeleteClusterInput{Name: aws.String(fx.ClusterName)}); err != nil {
		var aerr awserr.Error
		if errors.As(err, &aerr) && strings.Contains(aerr.Code(), "ResourceNotFound") {
			return
		}
		t.Logf("cleanup delete-cluster %s: %v", fx.ClusterName, err)
		return
	}
	harness.WaitForEKSClusterDeleted(t, c, fx.ClusterName)
}

// --- kubeconfig artifact --------------------------------------------------

// assertEKSEndpointResolves confirms the DescribeCluster endpoint is an
// AWS-shaped DNS name that resolves through the host resolver (the path a
// kubectl/AWS SDK client uses), matching real EKS. Skipped when northstar is not
// configured and the endpoint is still a bare IP. Retries because the endpoint A
// record is published asynchronously by the control-plane writer.
func assertEKSEndpointResolves(t *testing.T, endpoint string) {
	t.Helper()
	require.NotEmpty(t, endpoint, "cluster endpoint must be set")
	u, err := url.Parse(endpoint)
	require.NoErrorf(t, err, "parse cluster endpoint %q", endpoint)
	host := u.Hostname()
	if net.ParseIP(host) != nil {
		t.Logf("endpoint %q is a bare IP — DNS registration off, skipping resolution check", endpoint)
		return
	}
	deadline := time.Now().Add(90 * time.Second)
	var lastErr error
	for time.Now().Before(deadline) {
		if addrs, lerr := net.LookupHost(host); lerr == nil && len(addrs) > 0 {
			t.Logf("EKS endpoint %s resolved to %v (northstar path)", host, addrs)
			return
		} else {
			lastErr = lerr
		}
		time.Sleep(3 * time.Second)
	}
	t.Fatalf("EKS endpoint host %q never resolved within 90s (last err=%v) — northstar did not serve the A record", host, lastErr)
}

// writeKubeconfig builds a kubeconfig from DescribeCluster output and writes it to the artifact
// dir. Avoids shelling to `aws eks update-kubeconfig` so the structure assertion is hermetic.
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
