//go:build e2e

package gpu

import (
	"os"
	"sync"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/tests/e2e/harness"
)

var (
	pkgFixOnce    sync.Once
	pkgFix        *Fixture
	pkgFixErr     error
	pkgSkipReason string
)

func TestMain(m *testing.M) {
	code := m.Run()
	if pkgFix != nil {
		if pkgFix.Harness != nil {
			pkgFix.Harness.Close()
		}
		if pkgFix.TmpDir != "" {
			_ = os.RemoveAll(pkgFix.TmpDir)
		}
	}
	os.Exit(code)
}

// Fixture carries per-process state shared across GPU passthrough tests.
type Fixture struct {
	Env             *harness.Env
	AWS             *harness.AWSClient
	Harness         *harness.Fixture
	TmpDir          string
	GPUInstanceType string // first GPU instance type advertised by the node, e.g. "g5.xlarge"
	AMIID           string // AMI ID of the imported ubuntu-26.04-nvidia-gpu-x86_64 image
}

func (f *Fixture) ArtifactDir(t *testing.T) string {
	t.Helper()
	return harness.ArtifactDir(t, f.Env)
}

// requireGPUFixture returns the package-scoped Fixture singleton, building it
// on first call. Skips the calling test when:
//   - SPINIFEX_E2E is unset
//   - no GPU instance types are advertised (node has no GPU or gpu_passthrough=false)
//
// The ubuntu base GPU AMI is resolved best-effort into Fixture.AMIID; tests that
// launch from it must additionally call requireBaseGPUAMI. The ECS test resolves
// its own spinifex-ecs-node-gpu AMI and does not need the base image.
func requireGPUFixture(t *testing.T) *Fixture {
	t.Helper()
	pkgFixOnce.Do(func() {
		if os.Getenv("SPINIFEX_E2E") == "" {
			return
		}
		env := harness.LoadEnv(t)
		if env.Mode != harness.ModeSingle {
			pkgSkipReason = "gpu suite requires SPINIFEX_MODE=single"
			return
		}
		// Guard against harness.NewAWSClient calling t.Fatal (which exits via
		// runtime.Goexit and corrupts the Once state for subsequent tests) when
		// no Spinifex node is running. ResolveCACert uses the same candidate
		// paths, so a failure here gives a clean skip with an actionable message.
		if _, err := harness.ResolveCACert(env); err != nil {
			pkgSkipReason = "no Spinifex node running — provision first: ansible-playbook ansible/playbooks/dev-reset.yml"
			return
		}
		awsCli := harness.NewAWSClient(t, env)

		gpuType, reason := discoverGPUInstanceType(awsCli)
		if reason != "" {
			pkgSkipReason = reason
			return
		}
		amiID := discoverBaseGPUAMI(awsCli) // empty if not imported; gated per-test

		h, err := harness.NewProcessFixture(awsCli)
		if err != nil {
			pkgFixErr = err
			return
		}
		tmpDir, err := os.MkdirTemp("", "gpu-pkgfix-*")
		if err != nil {
			pkgFixErr = err
			return
		}
		harness.EnsureDefaultSGOpen(t, awsCli)
		pkgFix = &Fixture{
			Env:             env,
			AWS:             awsCli,
			Harness:         h,
			TmpDir:          tmpDir,
			GPUInstanceType: gpuType,
			AMIID:           amiID,
		}
	})
	if pkgFixErr != nil {
		t.Fatalf("gpu fixture init: %v", pkgFixErr)
	}
	if pkgFix == nil {
		if pkgSkipReason != "" {
			t.Skip(pkgSkipReason)
		}
		t.Skip("SPINIFEX_E2E unset")
	}
	return pkgFix
}

// discoverGPUInstanceType returns the first GPU instance type advertised by the
// node. A non-empty reason means none are advertised and the suite should skip.
func discoverGPUInstanceType(c *harness.AWSClient) (gpuType, reason string) {
	typesOut, err := c.EC2.DescribeInstanceTypes(&ec2.DescribeInstanceTypesInput{})
	if err != nil {
		return "", "DescribeInstanceTypes: " + err.Error()
	}
	for _, it := range typesOut.InstanceTypes {
		if it.GpuInfo != nil && len(it.GpuInfo.Gpus) > 0 {
			return aws.StringValue(it.InstanceType), ""
		}
	}
	return "", "no GPU instance types advertised — node has no GPU or gpu_passthrough is disabled"
}

// discoverBaseGPUAMI returns the ubuntu-26.04-nvidia-gpu-x86_64 image ID, or ""
// if it has not been imported. Only the raw-EC2 GPU tests launch from it.
func discoverBaseGPUAMI(c *harness.AWSClient) string {
	imgsOut, err := c.EC2.DescribeImages(&ec2.DescribeImagesInput{
		Filters: []*ec2.Filter{
			{Name: aws.String("name"), Values: []*string{aws.String("ubuntu-26.04-nvidia-gpu-x86_64")}},
			{Name: aws.String("state"), Values: []*string{aws.String("available")}},
		},
	})
	if err != nil || len(imgsOut.Images) == 0 {
		return ""
	}
	return aws.StringValue(imgsOut.Images[0].ImageId)
}

// requireBaseGPUAMI skips the calling test unless the ubuntu base GPU image is
// imported. Tests that launch a VM directly from the base AMI call this after
// requireGPUFixture; the ECS test uses its own node AMI and does not.
func requireBaseGPUAMI(t *testing.T, fix *Fixture) {
	t.Helper()
	if fix.AMIID == "" {
		t.Skip("ubuntu-26.04-nvidia-gpu-x86_64 AMI not imported — run: spx admin images import --name ubuntu-26.04-nvidia-gpu-x86_64")
	}
}
