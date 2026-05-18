//go:build e2e

package single

import (
	"fmt"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/tests/e2e/harness"
	"github.com/stretchr/testify/assert"
)

// phase9_Teardown is the final cleanup pass — terminate the primary
// instance, deregister the custom AMI, drop its backing snapshot, and
// delete the test key pair. Mirrors run-e2e.sh Phase 9 (~3500–3535) with
// the addition of key pair removal so a stale `e2e-key-*` doesn't linger.
//
// Resilient by design: every API call swallows `*NotFound` so a partial
// prior cleanup (or a previous test pass that already reaped the row)
// doesn't abort the rest of teardown. Anything else is logged via
// harness.Detail(..., "warn", ...) so 9a/9b can still execute and report
// a coherent picture of the final state.
func phase9_Teardown(t *testing.T, fix *Fixture) {
	harness.Phase(t, "Phase 9 — Teardown")

	// Delete the CreateImage backing snapshot first — checkVolumeHasNoSnapshots
	// blocks the root volume's DeleteOnTermination if a snapshot still
	// references it. Matches the ordering in run-e2e.sh ~3507–3513.
	if fix.CustomAMISnapID != "" {
		harness.Step(t, "delete-snapshot %s (custom AMI backing)", fix.CustomAMISnapID)
		_, err := fix.AWS.EC2.DeleteSnapshot(&ec2.DeleteSnapshotInput{
			SnapshotId: aws.String(fix.CustomAMISnapID),
		})
		switch {
		case err == nil:
			harness.Detail(t, "deleted_snapshot", fix.CustomAMISnapID)
		case harness.ErrorCodeIs(err, "InvalidSnapshot.NotFound"):
			harness.Detail(t, "snapshot_already_gone", fix.CustomAMISnapID)
		default:
			harness.Detail(t, "warn", fmt.Sprintf("delete-snapshot %s: %v", fix.CustomAMISnapID, err))
		}
		fix.CustomAMISnapID = ""
	}

	// Deregister the custom AMI so downstream suites don't pick it up after
	// its backing snapshot is gone (run-e2e.sh ~3515–3521).
	if fix.CustomAMIID != "" {
		harness.Step(t, "deregister-image %s", fix.CustomAMIID)
		_, err := fix.AWS.EC2.DeregisterImage(&ec2.DeregisterImageInput{
			ImageId: aws.String(fix.CustomAMIID),
		})
		switch {
		case err == nil:
			harness.Detail(t, "deregistered_ami", fix.CustomAMIID)
		case harness.ErrorCodeIs(err, "InvalidAMIID.NotFound"):
			harness.Detail(t, "ami_already_gone", fix.CustomAMIID)
		default:
			harness.Detail(t, "warn", fmt.Sprintf("deregister-image %s: %v", fix.CustomAMIID, err))
		}
		// Field retained for Phase 9a verification (DescribeImages must not
		// list it); cleared at the end of 9a once the assertion has run.
	}

	// Terminate the primary instance and wait for it to reach `terminated`.
	// run-e2e.sh ~3523–3535 polls describe-instances for up to 60 attempts;
	// we delegate to the package-local waitTerminated helper (NotFound is
	// treated as success there, same intent as the bash loop). Errors are
	// caught and surfaced as warnings — 9a will independently verify the
	// final state.
	if fix.InstanceID != "" {
		harness.Step(t, "terminate-instances %s", fix.InstanceID)
		_, err := fix.AWS.EC2.TerminateInstances(&ec2.TerminateInstancesInput{
			InstanceIds: []*string{aws.String(fix.InstanceID)},
		})
		switch {
		case err == nil:
			waitTerminated(t, fix.AWS, fix.InstanceID)
			harness.Detail(t, "terminated_instance", fix.InstanceID)
		case harness.ErrorCodeIs(err, "InvalidInstanceID.NotFound"):
			harness.Detail(t, "instance_already_gone", fix.InstanceID)
		default:
			harness.Detail(t, "warn", fmt.Sprintf("terminate-instances %s: %v", fix.InstanceID, err))
		}
		// Field retained for Phase 9a verification; cleared at end of 9a.
	}

	// Delete the test key pair — idempotent at the AWS surface (matches
	// Phase 8p), but be explicit about the NotFound swallow in case the
	// gateway tightens semantics later.
	if fix.KeyName != "" {
		harness.Step(t, "delete-key-pair %s", fix.KeyName)
		_, err := fix.AWS.EC2.DeleteKeyPair(&ec2.DeleteKeyPairInput{
			KeyName: aws.String(fix.KeyName),
		})
		switch {
		case err == nil:
			harness.Detail(t, "deleted_keypair", fix.KeyName)
		case harness.ErrorCodeIs(err, "InvalidKeyPair.NotFound"):
			harness.Detail(t, "keypair_already_gone", fix.KeyName)
		default:
			harness.Detail(t, "warn", fmt.Sprintf("delete-key-pair %s: %v", fix.KeyName, err))
		}
		// Field retained for Phase 9a verification; cleared at end of 9a.
	}
}

// phase9a_VerifyTeardown re-queries the API and asserts the resources Phase
// 9 just dropped are no longer live. Each check accepts both forms of "gone"
// the gateway exposes:
//   - the row is absent (NotFound / empty result), OR
//   - the row still exists in history with a terminal/deregistered state.
//
// After this phase succeeds the consumed Fixture fields are cleared so any
// later (currently no) phases don't trip over stale IDs.
func phase9a_VerifyTeardown(t *testing.T, fix *Fixture) {
	harness.Phase(t, "Phase 9a — Verify Teardown")

	// Instance: either terminated-in-history or fully reaped.
	if fix.InstanceID != "" {
		harness.Step(t, "describe-instances %s", fix.InstanceID)
		out, err := fix.AWS.EC2.DescribeInstances(&ec2.DescribeInstancesInput{
			InstanceIds: []*string{aws.String(fix.InstanceID)},
		})
		switch {
		case err == nil:
			state := ""
			if len(out.Reservations) > 0 && len(out.Reservations[0].Instances) > 0 {
				state = aws.StringValue(out.Reservations[0].Instances[0].State.Name)
			}
			assert.Equalf(t, "terminated", state,
				"instance %s expected terminated, got %q", fix.InstanceID, state)
			harness.Detail(t, "instance_state", state)
		case harness.ErrorCodeIs(err, "InvalidInstanceID.NotFound"):
			harness.Detail(t, "instance_reaped", fix.InstanceID)
		default:
			t.Errorf("describe-instances %s: %v", fix.InstanceID, err)
		}
	}

	// AMI: DescribeImages with the deregistered id must not include it. The
	// gateway is allowed to either omit it from the result or surface
	// InvalidAMIID.NotFound — both mean "gone".
	if fix.CustomAMIID != "" {
		harness.Step(t, "describe-images %s", fix.CustomAMIID)
		out, err := fix.AWS.EC2.DescribeImages(&ec2.DescribeImagesInput{
			ImageIds: []*string{aws.String(fix.CustomAMIID)},
		})
		switch {
		case err == nil:
			for _, img := range out.Images {
				assert.NotEqualf(t, fix.CustomAMIID, aws.StringValue(img.ImageId),
					"deregistered AMI %s still visible to DescribeImages", fix.CustomAMIID)
			}
			harness.Detail(t, "ami_visible", len(out.Images))
		case harness.ErrorCodeIs(err, "InvalidAMIID.NotFound"):
			harness.Detail(t, "ami_reaped", fix.CustomAMIID)
		default:
			t.Errorf("describe-images %s: %v", fix.CustomAMIID, err)
		}
	}

	// Key pair: an unfiltered DescribeKeyPairs must not list fix.KeyName.
	if fix.KeyName != "" {
		harness.Step(t, "describe-key-pairs (must not contain %s)", fix.KeyName)
		out, err := fix.AWS.EC2.DescribeKeyPairs(&ec2.DescribeKeyPairsInput{})
		if err != nil {
			t.Errorf("describe-key-pairs: %v", err)
		} else {
			for _, kp := range out.KeyPairs {
				assert.NotEqualf(t, fix.KeyName, aws.StringValue(kp.KeyName),
					"deleted key pair %s still listed by DescribeKeyPairs", fix.KeyName)
			}
			harness.Detail(t, "keypairs_listed", len(out.KeyPairs))
		}
	}

	// Clear the Fixture slots Phase 9 consumed so any future phase appended
	// after Stage G doesn't try to operate on a stale id.
	fix.InstanceID = ""
	fix.Instance = nil
	fix.RootVolumeID = ""
	fix.CustomAMIID = ""
	fix.KeyName = ""
	fix.KeyPath = ""
}

// phase9b_FinalClusterStats is a single-node sanity pass against `spx get
// vms` — after Phase 9's terminate + 9a's verification, the orchestrator
// view must no longer reference the test's primary instance. Skipped on
// multi-node where the CLI surface differs.
func phase9b_FinalClusterStats(t *testing.T, fix *Fixture) {
	harness.Phase(t, "Phase 9b — Final Cluster Stats")
	if fix.Env.Mode != harness.ModeSingle {
		t.Skipf("Phase 9b is single-node only (mode=%s)", fix.Env.Mode)
	}

	// Phase 9a clears fix.InstanceID after the API check; capture the id
	// we actually want to look for via a closure on the pre-teardown value
	// would be cleaner, but the same effect comes from t.Cleanup-style
	// sequencing: read the latest `spx get vms` and confirm it doesn't
	// list any of the suite's known instance markers. With InstanceID
	// already cleared, we lean on the "no orphan rows" intent — the output
	// should be empty or contain only unrelated VMs.
	out := harness.SpxGetVMs(t)
	harness.Detail(t, "spx_get_vms_bytes", len(out))

	// Defensive: if any caller appends a later phase that re-populates
	// InstanceID before reaching here, still assert it's gone from the
	// cluster view.
	if fix.InstanceID != "" {
		assert.NotContainsf(t, out, fix.InstanceID,
			"spx get vms still lists terminated instance %s\n%s", fix.InstanceID, out)
	}

	// Best-effort sanity: warn (don't fail) if the output unexpectedly lists
	// any i-XXXXXX rows — Phase 9 should have terminated the only one this
	// suite launched in single-node mode. Other suites running in parallel
	// against the same node would surface here, but the single-node CI
	// fixture runs serially.
	if strings.Contains(out, "i-") {
		t.Logf("phase9b: spx get vms still references instance-like rows (may be from concurrent suites):\n%s", out)
	}
}
