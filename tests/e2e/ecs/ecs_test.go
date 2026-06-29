//go:build e2e

package ecs

import (
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/awserr"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/aws/aws-sdk-go/service/ecs"
	"github.com/aws/aws-sdk-go/service/elbv2"
	"github.com/aws/aws-sdk-go/service/iam"
	"github.com/aws/aws-sdk-go/service/sts"
	"github.com/mulgadc/spinifex/tests/e2e/harness"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	ecsVPCCIDR     = "10.41.0.0/16"
	ecsSubnetACIDR = "10.41.1.0/24"
	ecsSubnetBCIDR = "10.41.2.0/24"
	ecsClusterPfx  = "ecs-e2e"
	ecsInstanceTyp = "t3.small"
	// awsvpc tasks serve HTTP on 80 (isolated ENI netns) so the ALB target group
	// health check (GET /) passes. bridge/host tasks share the host netns, where
	// the agent credential endpoint owns 169.254.170.2:80, so they run a durable
	// no-port command instead and only assert placement + mode, not serving.
	ecsTaskImage    = "docker.io/library/nginx:1.27-alpine"
	ecsHostNetnsCmd = "sleep 600"
)

// TestECS drives the ECS data plane end-to-end against the local awsgw: a
// customer VPC + a real container instance launched from the spinifex-ecs-node
// AMI (which boots, registers over the gateway, and runs tasks through
// containerd), then standalone tasks in all three network modes (awsvpc,
// bridge, host) and an awsvpc service fronted by an ALB target group.
//
// One fixture (VPC + cluster + one node) is shared across subtests — node boot
// + registration is the slow step, so re-provisioning per subtest would blow
// the suite timeout.
func TestECS(t *testing.T) {
	env := harness.LoadEnv(t)
	artifacts := harness.ArtifactDir(t, env)
	c := harness.NewAWSClient(t, env)

	fx := setupECSFixture(t, c, env, artifacts)

	t.Run("CreateCluster", func(t *testing.T) {
		cl := waitClusterActive(t, c, fx.ClusterName)
		assert.Equal(t, "ACTIVE", aws.StringValue(cl.Status))
		assert.Contains(t, aws.StringValue(cl.ClusterArn), ":cluster/"+fx.ClusterName)
	})

	t.Run("RegisterContainerInstance", func(t *testing.T) {
		ci := waitContainerInstanceActive(t, c, fx.ClusterName, fx.InstanceID)
		assert.Equal(t, "ACTIVE", aws.StringValue(ci.Status))
		assert.True(t, aws.BoolValue(ci.AgentConnected), "agent must be connected")
		assert.Greater(t, registeredResource(ci, "CPU"), int64(0), "node must register CPU capacity")
		assert.Greater(t, registeredResource(ci, "MEMORY"), int64(0), "node must register memory capacity")
	})

	t.Run("TaskAwsvpc", func(t *testing.T) {
		tdArn := registerTaskDef(t, c, fx, "awsvpc", nil)
		task := runStandaloneTask(t, c, fx, tdArn, &ecs.NetworkConfiguration{
			AwsvpcConfiguration: &ecs.AwsVpcConfiguration{
				Subnets:        aws.StringSlice([]string{fx.SubnetAID}),
				SecurityGroups: aws.StringSlice([]string{fx.SGID}),
			},
		})
		require.Len(t, task.Attachments, 1, "awsvpc task must have one ENI attachment")
		att := task.Attachments[0]
		assert.Equal(t, "ElasticNetworkInterface", aws.StringValue(att.Type))
		assert.NotEmpty(t, attachmentDetail(att, "privateIPv4Address"), "ENI must carry a private IP")
		assert.NotEmpty(t, attachmentDetail(att, "networkInterfaceId"), "ENI must carry an interface id")
	})

	t.Run("TaskBridge", func(t *testing.T) {
		tdArn := registerTaskDef(t, c, fx, "bridge", strings.Fields(ecsHostNetnsCmd))
		task := runStandaloneTask(t, c, fx, tdArn, nil)
		assert.Empty(t, task.Attachments, "bridge task must not allocate an ENI")
	})

	t.Run("TaskHost", func(t *testing.T) {
		tdArn := registerTaskDef(t, c, fx, "host", strings.Fields(ecsHostNetnsCmd))
		task := runStandaloneTask(t, c, fx, tdArn, nil)
		assert.Empty(t, task.Attachments, "host task must not allocate an ENI")
	})

	t.Run("ServiceWithELB", func(t *testing.T) {
		runServiceWithELB(t, c, env, fx)
	})
}

// --- Fixture --------------------------------------------------------------

type ecsFixture struct {
	ClusterName string
	AccountID   string
	Region      string
	VPCID       string
	SubnetAID   string
	SubnetBID   string
	IGWID       string
	RTID        string
	SGID        string
	TaskRoleARN string
	InstanceID  string
	KeyName     string
}

func setupECSFixture(t *testing.T, c *harness.AWSClient, env *harness.Env, artifacts string) *ecsFixture {
	t.Helper()
	fx := &ecsFixture{
		ClusterName: fmt.Sprintf("%s-%d", ecsClusterPfx, time.Now().Unix()),
		Region:      envOr("SPINIFEX_AWS_REGION", "ap-southeast-2"),
	}

	harness.Phase(t, "Resolving caller account")
	ident, err := c.STS.GetCallerIdentity(&sts.GetCallerIdentityInput{})
	require.NoError(t, err, "sts get-caller-identity")
	fx.AccountID = aws.StringValue(ident.Account)
	t.Logf("account: %s", fx.AccountID)

	harness.Phase(t, "Creating VPC topology (%s)", ecsVPCCIDR)
	createNetwork(t, c, fx)

	harness.Phase(t, "Ensuring ecsInstanceRole instance profile")
	ensureInstanceProfile(t, c)

	harness.Phase(t, "Creating task role")
	createTaskRole(t, c, fx)

	harness.Phase(t, "Creating cluster %q", fx.ClusterName)
	_, err = c.ECS.CreateCluster(&ecs.CreateClusterInput{ClusterName: aws.String(fx.ClusterName)})
	require.NoError(t, err, "create-cluster")
	t.Cleanup(func() {
		if _, derr := c.ECS.DeleteCluster(&ecs.DeleteClusterInput{Cluster: aws.String(fx.ClusterName)}); derr != nil {
			t.Logf("cleanup delete-cluster %s: %v", fx.ClusterName, derr)
		}
	})
	waitClusterActive(t, c, fx.ClusterName)

	harness.Phase(t, "Launching container instance")
	launchContainerInstance(t, c, env, fx, artifacts)

	harness.OnFailure(t, func() { dumpECS(t, c, artifacts, fx) })
	return fx
}

// --- Network --------------------------------------------------------------

func createNetwork(t *testing.T, c *harness.AWSClient, fx *ecsFixture) {
	t.Helper()

	vpc, err := c.EC2.CreateVpc(&ec2.CreateVpcInput{CidrBlock: aws.String(ecsVPCCIDR)}) // e2e:allow-create — the ECS fixture VPC.
	require.NoError(t, err, "create-vpc")
	fx.VPCID = aws.StringValue(vpc.Vpc.VpcId)
	t.Cleanup(func() {
		if _, derr := c.EC2.DeleteVpc(&ec2.DeleteVpcInput{VpcId: aws.String(fx.VPCID)}); derr != nil {
			t.Logf("delete vpc %s: %v", fx.VPCID, derr)
		}
	})

	igw, err := c.EC2.CreateInternetGateway(&ec2.CreateInternetGatewayInput{}) // e2e:allow-create
	require.NoError(t, err, "create-internet-gateway")
	fx.IGWID = aws.StringValue(igw.InternetGateway.InternetGatewayId)
	_, err = c.EC2.AttachInternetGateway(&ec2.AttachInternetGatewayInput{
		InternetGatewayId: aws.String(fx.IGWID), VpcId: aws.String(fx.VPCID),
	})
	require.NoError(t, err, "attach-internet-gateway")
	t.Cleanup(func() {
		if _, derr := c.EC2.DetachInternetGateway(&ec2.DetachInternetGatewayInput{
			InternetGatewayId: aws.String(fx.IGWID), VpcId: aws.String(fx.VPCID),
		}); derr != nil {
			t.Logf("detach igw %s: %v", fx.IGWID, derr)
		}
		if _, derr := c.EC2.DeleteInternetGateway(&ec2.DeleteInternetGatewayInput{
			InternetGatewayId: aws.String(fx.IGWID),
		}); derr != nil {
			t.Logf("delete igw %s: %v", fx.IGWID, derr)
		}
	})

	fx.SubnetAID = createSubnet(t, c, fx.VPCID, ecsSubnetACIDR)
	fx.SubnetBID = createSubnet(t, c, fx.VPCID, ecsSubnetBCIDR)

	rt, err := c.EC2.CreateRouteTable(&ec2.CreateRouteTableInput{VpcId: aws.String(fx.VPCID)}) // e2e:allow-create
	require.NoError(t, err, "create-route-table")
	fx.RTID = aws.StringValue(rt.RouteTable.RouteTableId)
	_, err = c.EC2.CreateRoute(&ec2.CreateRouteInput{
		RouteTableId:         aws.String(fx.RTID),
		DestinationCidrBlock: aws.String("0.0.0.0/0"),
		GatewayId:            aws.String(fx.IGWID),
	})
	require.NoError(t, err, "create-route")
	// Register the route-table delete before the association cleanups so LIFO
	// drains disassociations first — a still-associated table is undeletable.
	t.Cleanup(func() {
		if _, derr := c.EC2.DeleteRouteTable(&ec2.DeleteRouteTableInput{RouteTableId: aws.String(fx.RTID)}); derr != nil {
			t.Logf("delete rt %s: %v", fx.RTID, derr)
		}
	})
	for _, sn := range []string{fx.SubnetAID, fx.SubnetBID} {
		assoc, aerr := c.EC2.AssociateRouteTable(&ec2.AssociateRouteTableInput{
			RouteTableId: aws.String(fx.RTID), SubnetId: aws.String(sn),
		})
		require.NoError(t, aerr, "associate-route-table")
		assocID := aws.StringValue(assoc.AssociationId)
		t.Cleanup(func() {
			if _, derr := c.EC2.DisassociateRouteTable(&ec2.DisassociateRouteTableInput{
				AssociationId: aws.String(assocID),
			}); derr != nil {
				t.Logf("disassociate rt %s: %v", assocID, derr)
			}
		})
	}

	sg, err := c.EC2.CreateSecurityGroup(&ec2.CreateSecurityGroupInput{ // e2e:allow-create — the ECS fixture SG.
		VpcId:       aws.String(fx.VPCID),
		GroupName:   aws.String(fx.ClusterName + "-sg"),
		Description: aws.String("ecs e2e: SSH + HTTP in, all out"),
	})
	require.NoError(t, err, "create-security-group")
	fx.SGID = aws.StringValue(sg.GroupId)
	_, err = c.EC2.AuthorizeSecurityGroupIngress(&ec2.AuthorizeSecurityGroupIngressInput{
		GroupId: aws.String(fx.SGID),
		IpPermissions: []*ec2.IpPermission{
			ingressTCP(22), ingressTCP(80),
		},
	})
	require.NoError(t, err, "authorize-ingress")
	t.Cleanup(func() {
		if _, derr := c.EC2.DeleteSecurityGroup(&ec2.DeleteSecurityGroupInput{GroupId: aws.String(fx.SGID)}); derr != nil {
			t.Logf("delete sg %s: %v", fx.SGID, derr)
		}
	})
	t.Logf("network: vpc=%s subnets=[%s,%s] sg=%s", fx.VPCID, fx.SubnetAID, fx.SubnetBID, fx.SGID)
}

func createSubnet(t *testing.T, c *harness.AWSClient, vpcID, cidr string) string {
	t.Helper()
	out, err := c.EC2.CreateSubnet(&ec2.CreateSubnetInput{ // e2e:allow-create
		VpcId: aws.String(vpcID), CidrBlock: aws.String(cidr),
	})
	require.NoError(t, err, "create-subnet %s", cidr)
	id := aws.StringValue(out.Subnet.SubnetId)
	_, err = c.EC2.ModifySubnetAttribute(&ec2.ModifySubnetAttributeInput{
		SubnetId:            aws.String(id),
		MapPublicIpOnLaunch: &ec2.AttributeBooleanValue{Value: aws.Bool(true)},
	})
	if err != nil {
		t.Logf("map-public-ip on %s: %v", id, err)
	}
	t.Cleanup(func() {
		if _, derr := c.EC2.DeleteSubnet(&ec2.DeleteSubnetInput{SubnetId: aws.String(id)}); derr != nil {
			t.Logf("delete subnet %s: %v", id, derr)
		}
	})
	return id
}

func ingressTCP(port int64) *ec2.IpPermission {
	return &ec2.IpPermission{
		IpProtocol: aws.String("tcp"),
		FromPort:   aws.Int64(port),
		ToPort:     aws.Int64(port),
		IpRanges:   []*ec2.IpRange{{CidrIp: aws.String("0.0.0.0/0")}},
	}
}

// --- IAM ------------------------------------------------------------------

// ensureInstanceProfile makes the account-global ecsInstanceRole exist so the
// node's agent can vend task credentials from IMDS. Idempotent: if the profile
// already exists (the console or a prior run created it) it is left in place and
// not torn down.
func ensureInstanceProfile(t *testing.T, c *harness.AWSClient) {
	t.Helper()
	const name = "ecsInstanceRole"
	if _, err := c.IAM.GetInstanceProfile(&iam.GetInstanceProfileInput{
		InstanceProfileName: aws.String(name),
	}); err == nil {
		t.Logf("instance profile %s already present", name)
		return
	}

	const trust = `{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Principal":{"Service":"ec2.amazonaws.com"},"Action":"sts:AssumeRole"}]}`
	_, err := c.IAM.CreateRole(&iam.CreateRoleInput{
		RoleName:                 aws.String(name),
		AssumeRolePolicyDocument: aws.String(trust),
	})
	tolerateExists(t, err, "create-role ecsInstanceRole")

	const policy = `{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Action":"ecs:*","Resource":"*"}]}`
	_, err = c.IAM.PutRolePolicy(&iam.PutRolePolicyInput{
		RoleName:       aws.String(name),
		PolicyName:     aws.String("ecsInstanceRolePolicy"),
		PolicyDocument: aws.String(policy),
	})
	require.NoError(t, err, "put-role-policy ecsInstanceRole")

	_, err = c.IAM.CreateInstanceProfile(&iam.CreateInstanceProfileInput{
		InstanceProfileName: aws.String(name),
	})
	tolerateExists(t, err, "create-instance-profile ecsInstanceRole")

	_, err = c.IAM.AddRoleToInstanceProfile(&iam.AddRoleToInstanceProfileInput{
		InstanceProfileName: aws.String(name),
		RoleName:            aws.String(name),
	})
	tolerateExists(t, err, "add-role-to-instance-profile ecsInstanceRole")

	t.Cleanup(func() {
		_, _ = c.IAM.RemoveRoleFromInstanceProfile(&iam.RemoveRoleFromInstanceProfileInput{
			InstanceProfileName: aws.String(name), RoleName: aws.String(name),
		})
		_, _ = c.IAM.DeleteInstanceProfile(&iam.DeleteInstanceProfileInput{InstanceProfileName: aws.String(name)})
		_, _ = c.IAM.DeleteRolePolicy(&iam.DeleteRolePolicyInput{
			RoleName: aws.String(name), PolicyName: aws.String("ecsInstanceRolePolicy"),
		})
		_, _ = c.IAM.DeleteRole(&iam.DeleteRoleInput{RoleName: aws.String(name)})
	})
}

func createTaskRole(t *testing.T, c *harness.AWSClient, fx *ecsFixture) {
	t.Helper()
	name := fx.ClusterName + "-task-role"
	const trust = `{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Principal":{"Service":"ecs-tasks.amazonaws.com"},"Action":"sts:AssumeRole"}]}`
	out, err := c.IAM.CreateRole(&iam.CreateRoleInput{
		RoleName:                 aws.String(name),
		AssumeRolePolicyDocument: aws.String(trust),
	})
	require.NoError(t, err, "create task role")
	fx.TaskRoleARN = aws.StringValue(out.Role.Arn)
	_, err = c.IAM.PutRolePolicy(&iam.PutRolePolicyInput{
		RoleName:       aws.String(name),
		PolicyName:     aws.String("task-policy"),
		PolicyDocument: aws.String(`{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Action":["s3:GetObject"],"Resource":"*"}]}`),
	})
	require.NoError(t, err, "put task role policy")
	t.Cleanup(func() {
		_, _ = c.IAM.DeleteRolePolicy(&iam.DeleteRolePolicyInput{
			RoleName: aws.String(name), PolicyName: aws.String("task-policy"),
		})
		_, _ = c.IAM.DeleteRole(&iam.DeleteRoleInput{RoleName: aws.String(name)})
	})
}

func tolerateExists(t *testing.T, err error, what string) {
	t.Helper()
	if err == nil {
		return
	}
	var aerr awserr.Error
	if errors.As(err, &aerr) && strings.Contains(aerr.Code(), "EntityAlreadyExists") {
		return
	}
	require.NoError(t, err, what)
}

// --- Container instance ---------------------------------------------------

func launchContainerInstance(t *testing.T, c *harness.AWSClient, env *harness.Env, fx *ecsFixture, artifacts string) {
	t.Helper()

	fx.KeyName = fx.ClusterName + "-node"
	kp, err := c.EC2.CreateKeyPair(&ec2.CreateKeyPairInput{KeyName: aws.String(fx.KeyName)}) // e2e:allow-create — RunInstances requires a key.
	require.NoError(t, err, "create-key-pair")
	keyPath := filepath.Join(artifacts, fx.KeyName+".pem")
	_ = os.WriteFile(keyPath, []byte(aws.StringValue(kp.KeyMaterial)), 0o600)
	t.Cleanup(func() {
		_, _ = c.EC2.DeleteKeyPair(&ec2.DeleteKeyPairInput{KeyName: aws.String(fx.KeyName)})
	})

	amiID := resolveECSNodeAMI(t, c)
	userData := guestUserData(t, env, fx)

	out, err := c.EC2.RunInstances(&ec2.RunInstancesInput{ // e2e:allow-create — the container instance is the subject under test.
		ImageId:          aws.String(amiID),
		InstanceType:     aws.String(ecsInstanceTyp),
		KeyName:          aws.String(fx.KeyName),
		SubnetId:         aws.String(fx.SubnetAID),
		SecurityGroupIds: aws.StringSlice([]string{fx.SGID}),
		MinCount:         aws.Int64(1),
		MaxCount:         aws.Int64(1),
		IamInstanceProfile: &ec2.IamInstanceProfileSpecification{
			Name: aws.String("ecsInstanceRole"),
		},
		UserData: aws.String(base64.StdEncoding.EncodeToString([]byte(userData))),
	})
	require.NoError(t, err, "run-instances")
	require.NotEmpty(t, out.Instances, "run-instances returned no instance")
	fx.InstanceID = aws.StringValue(out.Instances[0].InstanceId)
	t.Cleanup(func() {
		_, _ = c.EC2.TerminateInstances(&ec2.TerminateInstancesInput{
			InstanceIds: aws.StringSlice([]string{fx.InstanceID}),
		})
		harness.WaitForInstanceTerminated(t, c, []string{fx.InstanceID}, 3*time.Minute)
	})
	t.Logf("container instance: %s (ami %s)", fx.InstanceID, amiID)
	harness.WaitForInstanceRunning(t, c, fx.InstanceID, 5*time.Minute)
}

// resolveECSNodeAMI picks the newest AMI tagged spinifex:managed-by=ecs.
func resolveECSNodeAMI(t *testing.T, c *harness.AWSClient) string {
	t.Helper()
	out, err := c.EC2.DescribeImages(&ec2.DescribeImagesInput{
		Filters: []*ec2.Filter{{
			Name:   aws.String("tag:spinifex:managed-by"),
			Values: aws.StringSlice([]string{"ecs"}),
		}},
	})
	require.NoError(t, err, "describe-images")
	require.NotEmpty(t, out.Images, "no spinifex-ecs-node AMI imported (tag spinifex:managed-by=ecs)")
	sort.Slice(out.Images, func(i, j int) bool {
		return aws.StringValue(out.Images[i].CreationDate) > aws.StringValue(out.Images[j].CreationDate)
	})
	return aws.StringValue(out.Images[0].ImageId)
}

// guestUserData builds the cloud-config the node boots with: the agent's gateway
// URL (LAN-reachable, never loopback) + the gateway CA, matching the
// ecs-quickstart workbook's user-data.
func guestUserData(t *testing.T, env *harness.Env, fx *ecsFixture) string {
	t.Helper()
	caPath, err := harness.ResolveCACert(env)
	require.NoError(t, err, "resolve CA cert")
	caBytes, err := os.ReadFile(caPath) //nolint:gosec // CA path from harness env
	require.NoError(t, err, "read CA cert")

	var ca strings.Builder
	for _, line := range strings.Split(strings.TrimRight(string(caBytes), "\n"), "\n") {
		ca.WriteString("      ")
		ca.WriteString(line)
		ca.WriteString("\n")
	}

	return fmt.Sprintf(`#cloud-config
write_files:
  - path: /etc/spinifex-ecs/agent.env
    permissions: '0600'
    content: |
      ECS_GATEWAY_URL=%s
      ECS_GATEWAY_CA=/etc/spinifex-ecs/gateway-ca.pem
      ECS_REGION=%s
      ECS_CLUSTER=%s
  - path: /etc/spinifex-ecs/gateway-ca.pem
    permissions: '0644'
    content: |
%s`, guestGatewayURL(env), fx.Region, fx.ClusterName, ca.String())
}

// guestGatewayURL returns a gateway URL the guest VM can reach. The awsgw bind
// IP (ServiceIPs[0]) is often an OVN-internal address unreachable from a guest,
// so prefer an explicit override, then the harness WAN host (the externally
// reachable node IP), and only fall back to a non-loopback node IP. Set
// SPINIFEX_ECS_GATEWAY_URL or SPINIFEX_WAN_IP when the default is wrong.
func guestGatewayURL(env *harness.Env) string {
	if u := os.Getenv("SPINIFEX_ECS_GATEWAY_URL"); u != "" {
		return u
	}
	candidates := append([]string{env.WANHost}, env.NodeIPs...)
	candidates = append(candidates, env.ServiceIPs...)
	for _, ip := range candidates {
		if ip != "" && !strings.HasPrefix(ip, "127.") {
			return fmt.Sprintf("https://%s:%d", ip, env.AWSGWPort)
		}
	}
	return fmt.Sprintf("https://127.0.0.1:%d", env.AWSGWPort)
}

func waitContainerInstanceActive(t *testing.T, c *harness.AWSClient, cluster, ec2ID string) *ecs.ContainerInstance {
	t.Helper()
	harness.Step(t, "waiting for %s to register in %s", ec2ID, cluster)
	var ci *ecs.ContainerInstance
	harness.EventuallyErr(t, func() error {
		list, err := c.ECS.ListContainerInstances(&ecs.ListContainerInstancesInput{Cluster: aws.String(cluster)})
		if err != nil {
			return fmt.Errorf("list-container-instances: %w", err)
		}
		if len(list.ContainerInstanceArns) == 0 {
			return fmt.Errorf("no container instances yet")
		}
		desc, err := c.ECS.DescribeContainerInstances(&ecs.DescribeContainerInstancesInput{
			Cluster: aws.String(cluster), ContainerInstances: list.ContainerInstanceArns,
		})
		if err != nil {
			return fmt.Errorf("describe-container-instances: %w", err)
		}
		for _, inst := range desc.ContainerInstances {
			if aws.StringValue(inst.Ec2InstanceId) == ec2ID && aws.StringValue(inst.Status) == "ACTIVE" {
				ci = inst
				return nil
			}
		}
		return fmt.Errorf("%s not ACTIVE yet", ec2ID)
	}, 4*time.Minute, 5*time.Second)
	harness.Step(t, "%s registered", ec2ID)
	return ci
}

// --- Task definitions & standalone tasks ----------------------------------

// registerTaskDef registers a one-container task definition. command overrides
// the image entrypoint; when set the container exposes no port (used by the
// host-netns modes to avoid the credential endpoint's :80). When command is nil
// the container runs nginx and publishes 80 for ALB/awsvpc serving.
func registerTaskDef(t *testing.T, c *harness.AWSClient, fx *ecsFixture, networkMode string, command []string) string {
	t.Helper()
	family := fmt.Sprintf("%s-%s", fx.ClusterName, networkMode)
	cdef := &ecs.ContainerDefinition{
		Name:      aws.String("web"),
		Image:     aws.String(ecsTaskImage),
		Cpu:       aws.Int64(128),
		Memory:    aws.Int64(256),
		Essential: aws.Bool(true),
	}
	if len(command) > 0 {
		cdef.Command = aws.StringSlice(command)
	} else {
		cdef.PortMappings = []*ecs.PortMapping{{
			ContainerPort: aws.Int64(80),
			Protocol:      aws.String("tcp"),
		}}
	}
	out, err := c.ECS.RegisterTaskDefinition(&ecs.RegisterTaskDefinitionInput{
		Family:                  aws.String(family),
		NetworkMode:             aws.String(networkMode),
		RequiresCompatibilities: aws.StringSlice([]string{"EC2"}),
		TaskRoleArn:             aws.String(fx.TaskRoleARN),
		ContainerDefinitions:    []*ecs.ContainerDefinition{cdef},
	})
	require.NoError(t, err, "register-task-definition %s", networkMode)
	arn := aws.StringValue(out.TaskDefinition.TaskDefinitionArn)
	t.Cleanup(func() {
		_, _ = c.ECS.DeregisterTaskDefinition(&ecs.DeregisterTaskDefinitionInput{TaskDefinition: aws.String(arn)})
	})
	return arn
}

func runStandaloneTask(t *testing.T, c *harness.AWSClient, fx *ecsFixture, tdArn string, netCfg *ecs.NetworkConfiguration) *ecs.Task {
	t.Helper()
	out, err := c.ECS.RunTask(&ecs.RunTaskInput{
		Cluster:              aws.String(fx.ClusterName),
		TaskDefinition:       aws.String(tdArn),
		Count:                aws.Int64(1),
		NetworkConfiguration: netCfg,
	})
	require.NoError(t, err, "run-task")
	require.NotEmpty(t, out.Tasks, "run-task returned no task")
	taskArn := aws.StringValue(out.Tasks[0].TaskArn)
	t.Cleanup(func() {
		_, _ = c.ECS.StopTask(&ecs.StopTaskInput{Cluster: aws.String(fx.ClusterName), Task: aws.String(taskArn)})
	})
	return waitTaskRunning(t, c, fx.ClusterName, taskArn)
}

func waitTaskRunning(t *testing.T, c *harness.AWSClient, cluster, taskArn string) *ecs.Task {
	t.Helper()
	harness.Step(t, "waiting for task %s to reach RUNNING", taskArn)
	var task *ecs.Task
	harness.EventuallyErr(t, func() error {
		out, err := c.ECS.DescribeTasks(&ecs.DescribeTasksInput{
			Cluster: aws.String(cluster), Tasks: aws.StringSlice([]string{taskArn}),
		})
		if err != nil {
			return fmt.Errorf("describe-tasks: %w", err)
		}
		if len(out.Tasks) == 0 {
			return fmt.Errorf("task not found")
		}
		task = out.Tasks[0]
		switch s := aws.StringValue(task.LastStatus); s {
		case "RUNNING":
			return nil
		case "STOPPED":
			return fmt.Errorf("task STOPPED: %s", aws.StringValue(task.StoppedReason))
		default:
			return fmt.Errorf("task status %s", s)
		}
	}, 6*time.Minute, 5*time.Second)

	// Guard against a transient RUNNING: a container that crash-loops on startup
	// (e.g. a port bind conflict) can flap through RUNNING for a few seconds.
	// Re-read after a short settle and fail if it has since STOPPED.
	time.Sleep(5 * time.Second)
	out, err := c.ECS.DescribeTasks(&ecs.DescribeTasksInput{
		Cluster: aws.String(cluster), Tasks: aws.StringSlice([]string{taskArn}),
	})
	require.NoError(t, err, "describe-tasks (settle)")
	require.NotEmpty(t, out.Tasks, "task vanished after RUNNING")
	task = out.Tasks[0]
	require.NotEqual(t, "STOPPED", aws.StringValue(task.LastStatus),
		"task did not stay RUNNING: %s", aws.StringValue(task.StoppedReason))

	harness.Step(t, "task %s RUNNING", taskArn)
	return task
}

// --- Service + ELB --------------------------------------------------------

func runServiceWithELB(t *testing.T, c *harness.AWSClient, env *harness.Env, fx *ecsFixture) {
	t.Helper()

	tg, err := c.ELBv2.CreateTargetGroup(&elbv2.CreateTargetGroupInput{ // e2e:allow-create — the service target group is the subject under test.
		Name:                       aws.String(shortName(fx.ClusterName + "-tg")),
		Port:                       aws.Int64(80),
		Protocol:                   aws.String("HTTP"),
		VpcId:                      aws.String(fx.VPCID),
		TargetType:                 aws.String("ip"),
		HealthCheckPath:            aws.String("/"),
		HealthCheckProtocol:        aws.String("HTTP"),
		HealthCheckIntervalSeconds: aws.Int64(10),
		HealthyThresholdCount:      aws.Int64(2),
	})
	require.NoError(t, err, "create-target-group")
	tgArn := aws.StringValue(tg.TargetGroups[0].TargetGroupArn)
	t.Cleanup(func() {
		_, _ = c.ELBv2.DeleteTargetGroup(&elbv2.DeleteTargetGroupInput{TargetGroupArn: aws.String(tgArn)})
	})

	lb, err := c.ELBv2.CreateLoadBalancer(&elbv2.CreateLoadBalancerInput{ // e2e:allow-create — the ELB-attached service is the subject under test.
		Name:           aws.String(shortName(fx.ClusterName + "-alb")),
		Type:           aws.String("application"),
		Scheme:         aws.String("internet-facing"),
		SecurityGroups: aws.StringSlice([]string{fx.SGID}),
		Subnets:        aws.StringSlice([]string{fx.SubnetAID, fx.SubnetBID}),
	})
	require.NoError(t, err, "create-load-balancer")
	lbArn := aws.StringValue(lb.LoadBalancers[0].LoadBalancerArn)
	t.Cleanup(func() {
		_, _ = c.ELBv2.DeleteLoadBalancer(&elbv2.DeleteLoadBalancerInput{LoadBalancerArn: aws.String(lbArn)})
	})
	harness.WaitForLBActive(t, c, lbArn, "ecs-alb", 3*time.Minute)

	lis, err := c.ELBv2.CreateListener(&elbv2.CreateListenerInput{ // e2e:allow-create — listener forwards to the service target group.
		LoadBalancerArn: aws.String(lbArn),
		Port:            aws.Int64(80),
		Protocol:        aws.String("HTTP"),
		DefaultActions: []*elbv2.Action{{
			Type: aws.String("forward"), TargetGroupArn: aws.String(tgArn),
		}},
	})
	require.NoError(t, err, "create-listener")
	lisArn := aws.StringValue(lis.Listeners[0].ListenerArn)
	t.Cleanup(func() {
		_, _ = c.ELBv2.DeleteListener(&elbv2.DeleteListenerInput{ListenerArn: aws.String(lisArn)})
	})

	tdArn := registerTaskDef(t, c, fx, "awsvpc", nil)
	svcName := fx.ClusterName + "-web"
	_, err = c.ECS.CreateService(&ecs.CreateServiceInput{
		Cluster:        aws.String(fx.ClusterName),
		ServiceName:    aws.String(svcName),
		TaskDefinition: aws.String(tdArn),
		DesiredCount:   aws.Int64(1),
		LaunchType:     aws.String("EC2"),
		NetworkConfiguration: &ecs.NetworkConfiguration{
			AwsvpcConfiguration: &ecs.AwsVpcConfiguration{
				Subnets:        aws.StringSlice([]string{fx.SubnetAID}),
				SecurityGroups: aws.StringSlice([]string{fx.SGID}),
				AssignPublicIp: aws.String("DISABLED"),
			},
		},
		LoadBalancers: []*ecs.LoadBalancer{{
			TargetGroupArn: aws.String(tgArn),
			ContainerName:  aws.String("web"),
			ContainerPort:  aws.Int64(80),
		}},
	})
	require.NoError(t, err, "create-service")
	t.Cleanup(func() {
		_, _ = c.ECS.UpdateService(&ecs.UpdateServiceInput{
			Cluster: aws.String(fx.ClusterName), Service: aws.String(svcName), DesiredCount: aws.Int64(0),
		})
		_, _ = c.ECS.DeleteService(&ecs.DeleteServiceInput{
			Cluster: aws.String(fx.ClusterName), Service: aws.String(svcName), Force: aws.Bool(true),
		})
	})

	harness.Step(t, "waiting for service runningCount to reach 1")
	harness.EventuallyErr(t, func() error {
		out, err := c.ECS.DescribeServices(&ecs.DescribeServicesInput{
			Cluster: aws.String(fx.ClusterName), Services: aws.StringSlice([]string{svcName}),
		})
		if err != nil {
			return fmt.Errorf("describe-services: %w", err)
		}
		if len(out.Services) == 0 {
			return fmt.Errorf("service not found")
		}
		if rc := aws.Int64Value(out.Services[0].RunningCount); rc < 1 {
			return fmt.Errorf("runningCount=%d", rc)
		}
		return nil
	}, 6*time.Minute, 5*time.Second)

	harness.WaitForTargetsHealthy(t, c, tgArn, 1, "ecs-service-tg", 4*time.Minute)

	// Best-effort: GET / through the ALB public IP. The .elb.spinifex.local DNS
	// name does not resolve from the host, so reach the assigned public IP.
	probeALBHTTP(t, c, lbArn)
}

func probeALBHTTP(t *testing.T, c *harness.AWSClient, lbArn string) {
	t.Helper()
	out, err := c.ELBv2.DescribeLoadBalancers(&elbv2.DescribeLoadBalancersInput{
		LoadBalancerArns: aws.StringSlice([]string{lbArn}),
	})
	if err != nil || len(out.LoadBalancers) == 0 {
		t.Logf("ALB HTTP probe skipped: describe failed: %v", err)
		return
	}
	var ip string
	for _, az := range out.LoadBalancers[0].AvailabilityZones {
		for _, addr := range az.LoadBalancerAddresses {
			if a := aws.StringValue(addr.IpAddress); a != "" {
				ip = a
			}
		}
	}
	if ip == "" {
		t.Logf("ALB HTTP probe skipped: no public IP assigned")
		return
	}
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Get("http://" + ip)
	if err != nil {
		t.Logf("ALB HTTP probe (http://%s) unreachable from host: %v", ip, err)
		return
	}
	defer func() { _ = resp.Body.Close() }()
	t.Logf("ALB HTTP probe http://%s -> %d", ip, resp.StatusCode)
	assert.Less(t, resp.StatusCode, 500, "ALB should not return 5xx")
}

// --- helpers --------------------------------------------------------------

func registeredResource(ci *ecs.ContainerInstance, name string) int64 {
	for _, r := range ci.RegisteredResources {
		if aws.StringValue(r.Name) == name {
			return aws.Int64Value(r.IntegerValue)
		}
	}
	return 0
}

func attachmentDetail(att *ecs.Attachment, key string) string {
	for _, kv := range att.Details {
		if aws.StringValue(kv.Name) == key {
			return aws.StringValue(kv.Value)
		}
	}
	return ""
}

func waitClusterActive(t *testing.T, c *harness.AWSClient, name string) *ecs.Cluster {
	t.Helper()
	var cl *ecs.Cluster
	harness.EventuallyErr(t, func() error {
		out, err := c.ECS.DescribeClusters(&ecs.DescribeClustersInput{Clusters: aws.StringSlice([]string{name})})
		if err != nil {
			return fmt.Errorf("describe-clusters: %w", err)
		}
		if len(out.Clusters) == 0 {
			return fmt.Errorf("cluster %s not found", name)
		}
		cl = out.Clusters[0]
		if aws.StringValue(cl.Status) != "ACTIVE" {
			return fmt.Errorf("cluster status %s", aws.StringValue(cl.Status))
		}
		return nil
	}, 1*time.Minute, 2*time.Second)
	return cl
}

func dumpECS(t *testing.T, c *harness.AWSClient, artifacts string, fx *ecsFixture) {
	list, err := c.ECS.ListTasks(&ecs.ListTasksInput{Cluster: aws.String(fx.ClusterName)})
	if err != nil {
		t.Logf("dumpECS list-tasks: %v", err)
		return
	}
	if len(list.TaskArns) == 0 {
		return
	}
	desc, err := c.ECS.DescribeTasks(&ecs.DescribeTasksInput{
		Cluster: aws.String(fx.ClusterName), Tasks: list.TaskArns,
	})
	if err != nil {
		t.Logf("dumpECS describe-tasks: %v", err)
		return
	}
	var b strings.Builder
	for _, task := range desc.Tasks {
		b.WriteString(task.String())
		b.WriteString("\n")
	}
	_ = os.WriteFile(filepath.Join(artifacts, "ecs-tasks-"+fx.ClusterName+".txt"), []byte(b.String()), 0o600)
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// shortName truncates an ELBv2 resource name to the 32-char API limit.
func shortName(s string) string {
	if len(s) <= 32 {
		return s
	}
	return s[len(s)-32:]
}
