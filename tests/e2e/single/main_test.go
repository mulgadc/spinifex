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
type Fixture struct {
	Env       *harness.Env
	AWS       *harness.AWSClient
	Artifacts string

	Arch         string // x86_64 | arm64
	AMIID        string // Phase 4
	InstanceType string // selected nano type (Phase 2)
	AZName       string // Phase 2
	KeyName      string // Phase 3
	KeyPath      string // PEM written by Phase 3

	Instance        *ec2.Instance // Phase 5 primary (Stage C populates)
	InstanceID      string
	RootVolumeID    string
	SSHHost         string
	SSHPort         int
	DefaultVPCID    string
	DefaultSGID     string
	DefaultSubnetID string

	VolumeID        string // Phase 5b
	SnapshotID      string // Phase 5c
	CopySnapshotID  string // Phase 5c
	CustomAMIID     string // Phase 5e
	CustomAMISnapID string // Phase 5e backing

	OVNAvailable bool // gates 8b-e
	PoolMode     bool // gates 8b / 8d
}

// TestSingleNode is the Go port of run-e2e.sh. Phases run as sequential
// subtests against a single shared Fixture — ordering matches the bash
// driver because Phase 5+ depend on the AMI/key/instance staged by 1–4.
func TestSingleNode(t *testing.T) {
	env := harness.LoadEnv(t)
	if env.Mode != harness.ModeSingle {
		t.Skipf("TestSingleNode requires SPINIFEX_MODE=single (got %q)", env.Mode)
	}

	fix := &Fixture{
		Env:       env,
		AWS:       harness.NewAWSClient(t, env),
		Artifacts: harness.ArtifactDir(t, env),
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

	// Stages E–G will append more t.Run calls here.

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
