//go:build e2e

// Package single is the Go port of run-e2e.sh — the single-node E2E suite
// that drives the full EC2/IAM lifecycle against a locally-bootstrapped
// Spinifex cluster. Each phase from the bash driver runs as a sequential
// subtest under TestSingleNode against one shared Fixture; ordering is
// contractual because Phase 5+ depend on the AMI / key pair / instance
// staged by Phase 1–4.
package single

import (
	"bufio"
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/tests/e2e/harness"
)

// Fixture carries state across the sequential Phase subtests of
// TestSingleNode. Mirrors the env vars run-e2e.sh threads between phases
// (AMI_ID, INSTANCE_ID, KEY_NAME, etc).
//
// Migration in progress: per-phase fields (AMIID, InstanceID, etc.) are
// being replaced by harness.Ensure* memoized lookups against Harness.
// Bead 3 (e2e-single-fixture-migration) removes each block as the
// downstream tests stop reading it; Bead 3h deletes the umbrella and
// shrinks Fixture to env + AWS client + Harness.
type Fixture struct {
	Env       *harness.Env
	AWS       *harness.AWSClient
	Harness   *harness.Fixture // memoized Ensure* fixture; parent test = TestSingleNode
	Artifacts string
	TmpDir    string // TestSingleNode-scoped scratch dir; survives until the whole test exits, unlike subtest t.TempDir().

	Arch         string // x86_64 | arm64
	AMIID        string // Phase 4
	InstanceType string // selected nano type (Phase 2)
	AZName       string // Phase 2
	KeyName      string // Phase 3
	KeyPath      string // PEM written by Phase 3

	Instance     *ec2.Instance // Phase 5 primary (Stage C populates)
	InstanceID   string
	RootVolumeID string
	SSHHost      string
	SSHPort      int

	VolumeID        string // Phase 5b
	SnapshotID      string // Phase 5c
	CopySnapshotID  string // Phase 5c
	CustomAMIID     string // Phase 5e
	CustomAMISnapID string // Phase 5e backing

	PoolMode bool // gates 8b / 8d

	// IAM phase state (Stage E). Threaded between IAM Phase 1–7 so
	// Phase 7's cleanup can defensively delete keys/users created
	// earlier even when an intermediate phase short-circuits.
	IAMAdminAccount  string // account ID extracted from CreatePolicy ARN
	IAMAliceKeyID    string
	IAMAliceSecret   string
	IAMBobKeyID      string
	IAMBobSecret     string
	IAMCharlieKeyID  string
	IAMCharlieSecret string
}

// TestSingleNode is the Go port of run-e2e.sh. Phases run as sequential
// subtests against a single shared Fixture — ordering matches the bash
// driver because Phase 5+ depend on the AMI/key/instance staged by 1–4.
func TestSingleNode(t *testing.T) {
	env := harness.LoadEnv(t)
	if env.Mode != harness.ModeSingle {
		t.Skipf("TestSingleNode requires SPINIFEX_MODE=single (got %q)", env.Mode)
	}

	awsClient := harness.NewAWSClient(t, env)
	fix := &Fixture{
		Env:       env,
		AWS:       awsClient,
		Harness:   harness.NewFixture(t, awsClient),
		Artifacts: harness.ArtifactDir(t, env),
		TmpDir:    t.TempDir(),
	}
	fix.PoolMode = detectPoolMode(env)

	// Phase ordering is contractual — do not parallelise.
	t.Run("Phase1_Environment", func(t *testing.T) { phase1_Environment(t, fix) })
	t.Run("Phase1b_ClusterStats", func(t *testing.T) { phase1b_ClusterStats(t, fix) })
	t.Run("Phase2_Discovery", func(t *testing.T) { phase2_Discovery(t, fix) })
	t.Run("Phase2b_SerialConsole", func(t *testing.T) { phase2b_SerialConsole(t, fix) })
	t.Run("Phase3_KeyPairs", func(t *testing.T) { phase3_KeyPairs(t, fix) })
	t.Run("Phase4_Image", func(t *testing.T) { phase4_Image(t, fix) })

	t.Run("Phase5_LaunchInstance", func(t *testing.T) { phase5_LaunchInstance(t, fix) })
	t.Run("Phase5a_pre_ClusterStats", func(t *testing.T) { phase5aPre_ClusterStats(t, fix) })
	t.Run("Phase5a_Metadata", func(t *testing.T) { phase5a_Metadata(t, fix) })
	t.Run("Phase5a_ii_SSH", func(t *testing.T) { phase5aii_SSHProbe(t, fix) })
	t.Run("Phase5a_iii_Console", func(t *testing.T) { phase5aiii_ConsoleOutput(t, fix) })
	t.Run("Phase5b_Volume", func(t *testing.T) { phase5b_VolumeLifecycle(t, fix) })
	t.Run("Phase5b_ii_VolumeStatus", func(t *testing.T) { phase5bii_VolumeStatus(t, fix) })
	t.Run("Phase5c_Snapshot", func(t *testing.T) { phase5c_SnapshotLifecycle(t, fix) })
	t.Run("Phase5d_SnapshotBackedLaunch", func(t *testing.T) { phase5d_SnapshotBackedLaunch(t, fix) })
	t.Run("Phase5e_CreateImage", func(t *testing.T) { phase5e_CreateImage(t, fix) })
	t.Run("Phase5f_SecurityGroupEgress", func(t *testing.T) { phase5f_SecurityGroupEgress(t, fix) })

	t.Run("Phase6_TagManagement", func(t *testing.T) { phase6_TagManagement(t, fix) })

	t.Run("Phase7_StopStart", func(t *testing.T) { phase7_StopStart(t, fix) })
	t.Run("Phase7a_AttachToStoppedError", func(t *testing.T) { phase7a_AttachToStoppedError(t, fix) })
	t.Run("Phase7b_ModifyInstanceAttribute", func(t *testing.T) { phase7b_ModifyInstanceAttribute(t, fix) })
	t.Run("Phase7c_pre_Reboot", func(t *testing.T) { phase7cPre_Reboot(t, fix) })
	t.Run("Phase7c_RunInstancesMultiCount", func(t *testing.T) { phase7c_RunInstancesMultiCount(t, fix) })

	t.Run("Phase8_NegativeErrorPaths", func(t *testing.T) { phase8_NegativeErrorPaths(t, fix) })

	t.Run("IAM1_UserCRUD", func(t *testing.T) { phaseIAM1_UserCRUD(t, fix) })
	t.Run("IAM2_AccessKeyLifecycle", func(t *testing.T) { phaseIAM2_AccessKeyLifecycle(t, fix) })
	t.Run("IAM3_UserAuthentication", func(t *testing.T) { phaseIAM3_UserAuthentication(t, fix) })
	t.Run("IAM4_PolicyCRUD", func(t *testing.T) { phaseIAM4_PolicyCRUD(t, fix) })
	t.Run("IAM5_PolicyAttachmentEnforcement", func(t *testing.T) { phaseIAM5_PolicyAttachmentEnforcement(t, fix) })
	t.Run("IAM6_PolicyLifecycle", func(t *testing.T) { phaseIAM6_PolicyLifecycle(t, fix) })
	t.Run("IAM7_Cleanup", func(t *testing.T) { phaseIAM7_Cleanup(t, fix) })

	t.Run("Phase8Acct_AccountScoping", func(t *testing.T) { phase8Acct_AccountScoping(t, fix) })

	t.Run("Phase8b_VPCSubnetE2E", func(t *testing.T) { phase8b_VPCSubnetE2E(t, fix) })
	t.Run("Phase8c_RouteTableValidation", func(t *testing.T) { phase8c_RouteTableValidation(t, fix) })
	t.Run("Phase8d_NATGateway", func(t *testing.T) { phase8d_NATGateway(t, fix) })
	t.Run("Phase8e_SGToSGDatapath", func(t *testing.T) { phase8e_SGToSGDatapath(t, fix) })

	t.Run("Phase9_Teardown", func(t *testing.T) { phase9_Teardown(t, fix) })
	t.Run("Phase9a_VerifyTeardown", func(t *testing.T) { phase9a_VerifyTeardown(t, fix) })
	t.Run("Phase9b_FinalClusterStats", func(t *testing.T) { phase9b_FinalClusterStats(t, fix) })

	harness.OnFailure(t, func() {
		harness.DumpCmd(t, fix.Artifacts, "ec2-describe-instances.txt",
			"aws", "ec2", "describe-instances")
	})
}

// detectPoolMode reads external_mode from spinifex.toml. Defaults to false
// (dev_networking) which is the single-node CI fixture. Mirrors the parser
// used by lb/lb_test.go's skipIfDevNetworking but reads the positive side —
// any non-empty external_mode value ("pool" / "nat") means external IPAM
// is in play.
func detectPoolMode(env *harness.Env) bool {
	cfg := os.ExpandEnv("$HOME/spinifex/config/spinifex.toml")
	if env.ConfigDir != "" {
		cfg = filepath.Join(env.ConfigDir, "spinifex.toml")
	}
	f, err := os.Open(cfg)
	if err != nil {
		return false
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	inNetwork := false
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if strings.HasPrefix(line, "[") {
			inNetwork = line == "[network]"
			continue
		}
		if !inNetwork {
			continue
		}
		if !strings.HasPrefix(line, "external_mode") {
			continue
		}
		// external_mode = "pool" — quoted value, anything non-empty == pool mode.
		if i := bytes.IndexByte([]byte(line), '='); i >= 0 {
			val := strings.TrimSpace(line[i+1:])
			val = strings.Trim(val, "\"'")
			return val != ""
		}
	}
	return false
}
