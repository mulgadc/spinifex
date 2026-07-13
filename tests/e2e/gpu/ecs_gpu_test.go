//go:build e2e

package gpu

import (
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/awserr"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/aws/aws-sdk-go/service/ecs"
	"github.com/aws/aws-sdk-go/service/iam"
	"github.com/mulgadc/spinifex/spinifex/instancetypes"
	"github.com/mulgadc/spinifex/spinifex/tags"
	"github.com/mulgadc/spinifex/tests/e2e/harness"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	ecsGPUClusterPfx = "ecs-gpu-e2e"

	// ecsGPUInstanceProfile is the account-global instance profile ECS nodes
	// assume for IMDS task-credential vending, matching tests/e2e/ecs/ecs_test.go's
	// ensureInstanceProfile (account-global, left in place across suites).
	ecsGPUInstanceProfile = "ecsInstanceRole"

	// nvidiaSmiContainerName names the single container in the GPU task def.
	nvidiaSmiContainerName = "nvidia-smi"
)

// ecsGPUFixture bundles the shared package fixture's GPU instance type with
// the ECS-tagged GPU node AMI resolved for it.
type ecsGPUFixture struct {
	Env          *harness.Env
	AWS          *harness.AWSClient
	Harness      *harness.Fixture
	InstanceType string
	AMIID        string
}

// requireECSGPUFixture skips the calling test unless every precondition for
// exercising Epic C (ECS: expose GPU to task containers) is met. It reuses
// requireGPUFixture's discovery (SPINIFEX_E2E, SPINIFEX_MODE=single, GPU
// instance type advertised) and additionally requires the ECS-tagged GPU node
// AMI (spinifex:managed-by=ecs, gpu-vendor=<vendor>) to be imported.
func requireECSGPUFixture(t *testing.T) *ecsGPUFixture {
	t.Helper()
	fix := requireGPUFixture(t)
	vendor := instancetypes.GPUVendorForType(fix.GPUInstanceType)
	amiID, reason := discoverECSGPUNodeAMI(fix.AWS, vendor)
	if reason != "" {
		t.Skip(reason)
	}
	return &ecsGPUFixture{
		Env: fix.Env, AWS: fix.AWS, Harness: fix.Harness,
		InstanceType: fix.GPUInstanceType, AMIID: amiID,
	}
}

// discoverECSGPUNodeAMI returns the newest AMI tagged spinifex:managed-by=ecs
// + gpu-vendor=vendor (the spinifex-ecs-node-gpu image), or a skip reason.
func discoverECSGPUNodeAMI(c *harness.AWSClient, vendor string) (amiID, reason string) {
	out, err := c.EC2.DescribeImages(&ec2.DescribeImagesInput{
		Filters: []*ec2.Filter{
			{Name: aws.String("tag:" + tags.ManagedByKey), Values: aws.StringSlice([]string{tags.ManagedByECS})},
			{Name: aws.String("tag:" + tags.GPUVendorKey), Values: aws.StringSlice([]string{vendor})},
			{Name: aws.String("state"), Values: aws.StringSlice([]string{"available"})},
		},
	})
	if err != nil {
		return "", "DescribeImages (ecs gpu ami): " + err.Error()
	}
	if len(out.Images) == 0 {
		return "", fmt.Sprintf("ECS GPU node AMI not imported (managed-by=ecs, gpu-vendor=%s) — run: spx admin images import --name spinifex-ecs-node-gpu", vendor)
	}
	sort.Slice(out.Images, func(i, j int) bool {
		return aws.StringValue(out.Images[i].CreationDate) > aws.StringValue(out.Images[j].CreationDate)
	})
	return aws.StringValue(out.Images[0].ImageId), ""
}

// TestECSGPUTaskExposure drives Epic C end-to-end: an ECS cluster plus a
// single GPU-capable container instance, then a task definition carrying a
// GPU resourceRequirement running nvidia-smi — asserting the task reaches
// RUNNING then STOPPED cleanly (nvidia-smi is one-shot), the container exits
// 0, and DescribeTasks reports the pinned GPU device UUID(s) (the C3
// report-back). Container stdout/stderr are not retrievable through this
// harness (no log-retrieval API wired for ECS), so exit code + gpuIds stand
// in for asserting on the nvidia-smi table text directly.
func TestECSGPUTaskExposure(t *testing.T) {
	fx := requireECSGPUFixture(t)
	artifacts := harness.ArtifactDir(t, fx.Env)

	harness.Phase(t, "Ensuring ecsInstanceRole instance profile")
	ensureECSGPUInstanceProfile(t, fx.AWS)

	vpc := harness.EnsureDefaultVPC(t, fx.Harness)
	harness.AuthorizeSSHIngress(t, fx.AWS, vpc.SGID)
	keyName, _ := harness.EnsureKeyPair(t, fx.Harness, artifacts)

	clusterName := fmt.Sprintf("%s-%d", ecsGPUClusterPfx, time.Now().Unix())
	harness.Phase(t, "Creating ECS cluster %q", clusterName)
	// e2e:allow-create — the GPU cluster is the subject under test.
	_, err := fx.AWS.ECS.CreateCluster(&ecs.CreateClusterInput{ClusterName: aws.String(clusterName)})
	require.NoError(t, err, "create-cluster")
	t.Cleanup(func() { deleteECSGPUClusterBestEffort(t, fx.AWS, clusterName) })
	waitECSGPUClusterActive(t, fx.AWS, clusterName)

	harness.Phase(t, "Launching GPU container instance (%s, %s)", fx.InstanceType, fx.AMIID)
	instanceID := launchECSGPUContainerInstance(t, fx, vpc, clusterName, keyName)
	harness.Detail(t, "instance", instanceID)

	ci := waitECSGPUContainerInstanceActive(t, fx.AWS, clusterName, instanceID)
	assert.True(t, aws.BoolValue(ci.AgentConnected), "GPU container instance agent must be connected")
	assert.Greater(t, registeredGPUResource(ci), int64(0), "container instance must register GPU capacity")

	harness.Phase(t, "Registering nvidia-smi GPU task definition")
	tdArn := registerECSGPUTaskDef(t, fx.AWS, clusterName)

	harness.Phase(t, "Running nvidia-smi GPU task")
	taskArn := runECSGPUTask(t, fx.AWS, clusterName, tdArn)

	task := waitECSGPUTaskStopped(t, fx.AWS, clusterName, taskArn, 6*time.Minute)
	require.Len(t, task.Containers, 1, "task must report exactly one container")
	ctr := task.Containers[0]
	assert.Equal(t, int64(0), aws.Int64Value(ctr.ExitCode),
		"nvidia-smi container exit code must be 0 (reason=%s, stoppedReason=%s)",
		aws.StringValue(ctr.Reason), aws.StringValue(task.StoppedReason))
	assert.NotEmpty(t, ctr.GpuIds, "container must report pinned GPU device UUID(s) (C3 report-back)")
	harness.Detail(t, "gpuIds", aws.StringValueSlice(ctr.GpuIds))
}

// registeredGPUResource returns the container instance's registered GPU
// count (STRINGSET length), or 0 if the resource is absent.
func registeredGPUResource(ci *ecs.ContainerInstance) int64 {
	for _, r := range ci.RegisteredResources {
		if aws.StringValue(r.Name) == "GPU" {
			return int64(len(aws.StringValueSlice(r.StringSetValue)))
		}
	}
	return 0
}

// ensureECSGPUInstanceProfile makes the account-global ecsInstanceRole exist
// so the GPU node's agent can vend task credentials from IMDS. Idempotent and
// account-global (shared with tests/e2e/ecs), so it is not torn down.
func ensureECSGPUInstanceProfile(t *testing.T, c *harness.AWSClient) {
	t.Helper()
	if _, err := c.IAM.GetInstanceProfile(&iam.GetInstanceProfileInput{
		InstanceProfileName: aws.String(ecsGPUInstanceProfile),
	}); err == nil {
		return
	}

	const trust = `{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Principal":{"Service":"ec2.amazonaws.com"},"Action":"sts:AssumeRole"}]}`
	_, err := c.IAM.CreateRole(&iam.CreateRoleInput{
		RoleName:                 aws.String(ecsGPUInstanceProfile),
		AssumeRolePolicyDocument: aws.String(trust),
	})
	tolerateEntityExists(t, err, "create-role ecsInstanceRole")

	const policy = `{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Action":"ecs:*","Resource":"*"}]}`
	_, err = c.IAM.PutRolePolicy(&iam.PutRolePolicyInput{
		RoleName: aws.String(ecsGPUInstanceProfile), PolicyName: aws.String("ecsInstanceRolePolicy"),
		PolicyDocument: aws.String(policy),
	})
	require.NoError(t, err, "put-role-policy ecsInstanceRole")

	_, err = c.IAM.CreateInstanceProfile(&iam.CreateInstanceProfileInput{
		InstanceProfileName: aws.String(ecsGPUInstanceProfile),
	})
	tolerateEntityExists(t, err, "create-instance-profile ecsInstanceRole")

	_, err = c.IAM.AddRoleToInstanceProfile(&iam.AddRoleToInstanceProfileInput{
		InstanceProfileName: aws.String(ecsGPUInstanceProfile), RoleName: aws.String(ecsGPUInstanceProfile),
	})
	tolerateEntityExists(t, err, "add-role-to-instance-profile ecsInstanceRole")
}

func tolerateEntityExists(t *testing.T, err error, what string) {
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

// launchECSGPUContainerInstance boots the GPU AMI as an ECS container
// instance, wired to register with clusterName over the local gateway.
func launchECSGPUContainerInstance(t *testing.T, fx *ecsGPUFixture, vpc harness.VPCInfo, clusterName, keyName string) string {
	t.Helper()
	userData := ecsGPUUserData(t, fx.Env, clusterName)
	out, err := fx.AWS.EC2.RunInstances(&ec2.RunInstancesInput{ // e2e:allow-create — the GPU container instance is the subject under test.
		ImageId:          aws.String(fx.AMIID),
		InstanceType:     aws.String(fx.InstanceType),
		KeyName:          aws.String(keyName),
		SubnetId:         aws.String(vpc.SubnetID),
		SecurityGroupIds: aws.StringSlice([]string{vpc.SGID}),
		MinCount:         aws.Int64(1),
		MaxCount:         aws.Int64(1),
		IamInstanceProfile: &ec2.IamInstanceProfileSpecification{
			Name: aws.String(ecsGPUInstanceProfile),
		},
		UserData: aws.String(base64.StdEncoding.EncodeToString([]byte(userData))),
	})
	require.NoError(t, err, "run-instances (ecs gpu node)")
	require.NotEmpty(t, out.Instances, "run-instances returned no instance")
	id := aws.StringValue(out.Instances[0].InstanceId)
	t.Cleanup(func() {
		_, _ = fx.AWS.EC2.TerminateInstances(&ec2.TerminateInstancesInput{
			InstanceIds: aws.StringSlice([]string{id}),
		})
		harness.WaitForInstanceState(t, fx.AWS, id, "terminated")
	})
	// GPU driver init on first boot adds latency beyond a plain node boot.
	harness.WaitForInstanceRunning(t, fx.AWS, id, 6*time.Minute)
	return id
}

// ecsGPUUserData builds the cloud-config the GPU node boots with: the
// agent's gateway URL and CA, matching tests/e2e/ecs/ecs_test.go's
// guestUserData (reimplemented locally — separate test package/binary).
func ecsGPUUserData(t *testing.T, env *harness.Env, cluster string) string {
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
%s`, ecsGPUGatewayURL(env), ecsGPURegion(), cluster, ca.String())
}

// ecsGPUGatewayURL resolves a gateway URL the guest VM can reach, preferring
// an explicit override then the harness WAN host, matching
// tests/e2e/ecs/ecs_test.go's guestGatewayURL.
func ecsGPUGatewayURL(env *harness.Env) string {
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

func ecsGPURegion() string {
	if v := os.Getenv("SPINIFEX_AWS_REGION"); v != "" {
		return v
	}
	return "ap-southeast-2"
}

func waitECSGPUClusterActive(t *testing.T, c *harness.AWSClient, name string) {
	t.Helper()
	harness.EventuallyErr(t, func() error {
		out, err := c.ECS.DescribeClusters(&ecs.DescribeClustersInput{Clusters: aws.StringSlice([]string{name})})
		if err != nil {
			return fmt.Errorf("describe-clusters: %w", err)
		}
		if len(out.Clusters) == 0 {
			return fmt.Errorf("cluster %s not found", name)
		}
		if s := aws.StringValue(out.Clusters[0].Status); s != "ACTIVE" {
			return fmt.Errorf("cluster status %s, want ACTIVE", s)
		}
		return nil
	}, 1*time.Minute, 2*time.Second)
}

func waitECSGPUContainerInstanceActive(t *testing.T, c *harness.AWSClient, cluster, ec2ID string) *ecs.ContainerInstance {
	t.Helper()
	harness.Step(t, "waiting for GPU container instance %s to register in %s", ec2ID, cluster)
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
	}, 6*time.Minute, 5*time.Second) // GPU driver init + agent registration
	harness.Step(t, "%s registered", ec2ID)
	return ci
}

// registerECSGPUTaskDef registers a single-container task def whose container
// carries a GPU resourceRequirement and runs nvidia-smi once.
func registerECSGPUTaskDef(t *testing.T, c *harness.AWSClient, clusterName string) string {
	t.Helper()
	family := clusterName + "-nvidia-smi"
	out, err := c.ECS.RegisterTaskDefinition(&ecs.RegisterTaskDefinitionInput{
		Family:                  aws.String(family),
		NetworkMode:             aws.String("bridge"),
		RequiresCompatibilities: aws.StringSlice([]string{"EC2"}),
		ContainerDefinitions: []*ecs.ContainerDefinition{{
			Name:      aws.String(nvidiaSmiContainerName),
			Image:     aws.String(nvidiaSmiPodImage),
			Cpu:       aws.Int64(128),
			Memory:    aws.Int64(512),
			Essential: aws.Bool(true),
			Command:   aws.StringSlice([]string{"nvidia-smi"}),
			ResourceRequirements: []*ecs.ResourceRequirement{{
				Type: aws.String("GPU"), Value: aws.String("1"),
			}},
		}},
	})
	require.NoError(t, err, "register-task-definition (nvidia-smi GPU)")
	arn := aws.StringValue(out.TaskDefinition.TaskDefinitionArn)
	t.Cleanup(func() {
		_, _ = c.ECS.DeregisterTaskDefinition(&ecs.DeregisterTaskDefinitionInput{TaskDefinition: aws.String(arn)})
	})
	return arn
}

func runECSGPUTask(t *testing.T, c *harness.AWSClient, cluster, tdArn string) string {
	t.Helper()
	out, err := c.ECS.RunTask(&ecs.RunTaskInput{
		Cluster:        aws.String(cluster),
		TaskDefinition: aws.String(tdArn),
		Count:          aws.Int64(1),
	})
	require.NoError(t, err, "run-task (nvidia-smi GPU)")
	require.NotEmpty(t, out.Tasks, "run-task returned no task")
	arn := aws.StringValue(out.Tasks[0].TaskArn)
	t.Cleanup(func() {
		_, _ = c.ECS.StopTask(&ecs.StopTaskInput{Cluster: aws.String(cluster), Task: aws.String(arn)})
	})
	return arn
}

// waitECSGPUTaskStopped polls until the task reaches STOPPED, tolerating (and
// noting) a RUNNING observation along the way — nvidia-smi is a one-shot
// command, so the task may transition RUNNING -> STOPPED between polls.
func waitECSGPUTaskStopped(t *testing.T, c *harness.AWSClient, cluster, taskArn string, timeout time.Duration) *ecs.Task {
	t.Helper()
	harness.Step(t, "waiting for task %s: RUNNING then STOPPED (nvidia-smi is one-shot)", taskArn)
	var task *ecs.Task
	sawRunning := false
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
		case "STOPPED":
			return nil
		case "RUNNING":
			sawRunning = true
			return fmt.Errorf("task RUNNING, waiting for STOPPED")
		default:
			return fmt.Errorf("task status %s", s)
		}
	}, timeout, 5*time.Second)
	harness.Detail(t, "sawRunning", sawRunning, "stoppedReason", aws.StringValue(task.StoppedReason))
	return task
}

func deleteECSGPUClusterBestEffort(t *testing.T, c *harness.AWSClient, cluster string) {
	t.Helper()
	if _, err := c.ECS.DeleteCluster(&ecs.DeleteClusterInput{Cluster: aws.String(cluster)}); err != nil {
		t.Logf("cleanup delete-cluster %s: %v", cluster, err)
	}
}
