//go:build e2e

package single

import (
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/tests/e2e/harness"
	"github.com/stretchr/testify/require"
)

// runNegativeErrorPaths ports run-e2e.sh Phase 8 (lines ~1298–1399):
// every sub-test exercises a single error code on the gateway. Each case
// runs in its own t.Run so one failure doesn't poison the others; cleanup
// is defensive (calls that unexpectedly succeed get torn down).
//
// Sub-tests:
//
//	8a InvalidAMIID.Malformed       — RunInstances with non-ami- image id
//	8b InvalidInstanceType          — RunInstances with bogus type
//	8c VolumeInUse                  — AttachVolume on root (already in-use)
//	8d OperationNotPermitted        — DetachVolume on root volume
//	8e InvalidSnapshot.NotFound     — DeleteSnapshot for unknown snap
//	8f InvalidAction (raw HTTP)     — unsupported gateway action
//	8g InvalidAMIID.NotFound        — RunInstances with bogus ami id
//	8h InvalidKeyPair.NotFound      — RunInstances with bogus key name
//	8i InvalidVolume.NotFound       — DeleteVolume for unknown vol
//	8j InvalidKeyPair.Duplicate     — CreateKeyPair re-using phase 3 key
//	8k InvalidKeyPair.Duplicate     — ImportKeyPair re-using phase 3 key
//	8l InvalidKey.Format            — ImportKeyPair with non-PEM/SSH material
//	8m InvalidVolume.NotFound       — DescribeVolumes with unknown vol id
//	8n InvalidAMIID.NotFound        — DescribeImages with unknown ami id
//	8o InvalidAMIName.Duplicate     — CreateImage with e2e-custom-ami name
//	8p (idempotent)                 — DeleteKeyPair for unknown key must succeed
//	8q InvalidInstanceID.NotFound   — ModifyInstanceAttribute on running inst
//	8r InvalidInstanceID.NotFound   — RebootInstances on unknown id
func runNegativeErrorPaths(t *testing.T, fix *Fixture) {
	harness.Phase(t, "Single — Negative / Error Path Tests")

	// Bootstrap every prereq up front so the parallel sub-tests below all
	// see populated locals.
	amiID := needAMI(t, fix)
	inst, rootVolumeID := needInstance(t, fix)
	instanceID := aws.StringValue(inst.InstanceId)
	instType, _ := needInstanceTypeArch(t, fix)
	customAMIID := needCustomAMI(t, fix)
	existingKey, _ := needKeyPair(t, fix)

	// 8a: RunInstances with malformed AMI ID (missing ami- prefix).
	t.Run("8a_InvalidAMIIDMalformed", func(t *testing.T) {
		t.Parallel()
		harness.Step(t, "run-instances image-id=notanami")
		out, err := fix.AWS.EC2.RunInstances(&ec2.RunInstancesInput{
			ImageId:      aws.String("notanami"),
			InstanceType: aws.String(instType),
			KeyName:      aws.String(existingKey),
			MinCount:     aws.Int64(1),
			MaxCount:     aws.Int64(1),
		})
		cleanupUnexpectedInstances(t, fix, out)
		harness.AssertAWSError(t, err, "InvalidAMIID.Malformed")
	})

	// 8b: RunInstances with invalid instance type.
	t.Run("8b_InvalidInstanceType", func(t *testing.T) {
		t.Parallel()
		harness.Step(t, "run-instances instance-type=x99.superlarge")
		out, err := fix.AWS.EC2.RunInstances(&ec2.RunInstancesInput{
			ImageId:      aws.String(amiID),
			InstanceType: aws.String("x99.superlarge"),
			KeyName:      aws.String(existingKey),
			MinCount:     aws.Int64(1),
			MaxCount:     aws.Int64(1),
		})
		cleanupUnexpectedInstances(t, fix, out)
		harness.AssertAWSError(t, err, "InvalidInstanceType")
	})

	// 8c: AttachVolume on the already-attached root volume.
	t.Run("8c_VolumeInUse", func(t *testing.T) {
		t.Parallel()
		harness.Step(t, "attach-volume %s (root, in-use)", rootVolumeID)
		_, err := fix.AWS.EC2.AttachVolume(&ec2.AttachVolumeInput{
			VolumeId:   aws.String(rootVolumeID),
			InstanceId: aws.String(instanceID),
			Device:     aws.String("/dev/sdg"),
		})
		harness.AssertAWSError(t, err, "VolumeInUse")
	})

	// 8d: DetachVolume against the boot/root volume — explicitly disallowed.
	t.Run("8d_DetachRootForbidden", func(t *testing.T) {
		t.Parallel()
		harness.Step(t, "detach-volume %s (root)", rootVolumeID)
		_, err := fix.AWS.EC2.DetachVolume(&ec2.DetachVolumeInput{
			VolumeId:   aws.String(rootVolumeID),
			InstanceId: aws.String(instanceID),
		})
		harness.AssertAWSError(t, err, "OperationNotPermitted")
	})

	// 8e: DeleteSnapshot on a non-existent snapshot id.
	t.Run("8e_DeleteSnapshotNotFound", func(t *testing.T) {
		t.Parallel()
		harness.Step(t, "delete-snapshot snap-nonexistent000000")
		_, err := fix.AWS.EC2.DeleteSnapshot(&ec2.DeleteSnapshotInput{
			SnapshotId: aws.String("snap-nonexistent000000"),
		})
		harness.AssertAWSError(t, err, "InvalidSnapshot.NotFound")
	})

	// 8f: Raw HTTP POST with an unsupported Action. The SDK refuses to build
	// requests for actions it doesn't know about, so this one bypasses it.
	// Bash treats InvalidAction / UnknownAction / generic "Error" as a pass;
	// here we accept either of the two canonical codes and surface anything
	// else so we notice regressions in the gateway error envelope.
	t.Run("8f_InvalidActionRawHTTP", func(t *testing.T) {
		t.Parallel()
		harness.Step(t, "POST Action=DescribeFakeThings")
		status, body, code := harness.PostAWSAction(t, fix.Env, fix.AWS,
			"DescribeFakeThings", nil)
		harness.Detail(t, "status", status, "code", code)
		switch code {
		case "InvalidAction", "UnknownAction":
			// expected
		default:
			t.Fatalf("expected InvalidAction/UnknownAction, got code=%q status=%d body=%s",
				code, status, string(body))
		}
	})

	// 8g: RunInstances with a well-formed but non-existent AMI id.
	t.Run("8g_InvalidAMIIDNotFound", func(t *testing.T) {
		t.Parallel()
		harness.Step(t, "run-instances image-id=ami-0000000000000dead")
		out, err := fix.AWS.EC2.RunInstances(&ec2.RunInstancesInput{
			ImageId:      aws.String("ami-0000000000000dead"),
			InstanceType: aws.String(instType),
			KeyName:      aws.String(existingKey),
			MinCount:     aws.Int64(1),
			MaxCount:     aws.Int64(1),
		})
		cleanupUnexpectedInstances(t, fix, out)
		harness.AssertAWSError(t, err, "InvalidAMIID.NotFound")
	})

	// 8h: RunInstances with non-existent key pair name.
	t.Run("8h_InvalidKeyPairNotFound", func(t *testing.T) {
		t.Parallel()
		harness.Step(t, "run-instances key-name=nonexistent-key-xyz")
		out, err := fix.AWS.EC2.RunInstances(&ec2.RunInstancesInput{
			ImageId:      aws.String(amiID),
			InstanceType: aws.String(instType),
			KeyName:      aws.String("nonexistent-key-xyz"),
			MinCount:     aws.Int64(1),
			MaxCount:     aws.Int64(1),
		})
		cleanupUnexpectedInstances(t, fix, out)
		harness.AssertAWSError(t, err, "InvalidKeyPair.NotFound")
	})

	// 8i: DeleteVolume on an unknown volume id.
	t.Run("8i_DeleteVolumeNotFound", func(t *testing.T) {
		t.Parallel()
		harness.Step(t, "delete-volume vol-0000000000000dead")
		_, err := fix.AWS.EC2.DeleteVolume(&ec2.DeleteVolumeInput{
			VolumeId: aws.String("vol-0000000000000dead"),
		})
		harness.AssertAWSError(t, err, "InvalidVolume.NotFound")
	})

	// 8j: CreateKeyPair re-using the Phase 3 primary key (still present).
	t.Run("8j_CreateKeyPairDuplicate", func(t *testing.T) {
		t.Parallel()
		harness.Step(t, "create-key-pair %s (duplicate)", existingKey)
		out, err := fix.AWS.EC2.CreateKeyPair(&ec2.CreateKeyPairInput{
			KeyName: aws.String(existingKey),
		})
		// If the gateway regresses and accepts the duplicate, the newly-issued
		// pair would shadow Phase 3's — defensively wipe so the rest of the
		// suite (and Stage G teardown) still sees the original.
		if err == nil && out != nil {
			t.Cleanup(func() {
				_, _ = fix.AWS.EC2.DeleteKeyPair(&ec2.DeleteKeyPairInput{
					KeyName: aws.String(existingKey),
				})
			})
		}
		harness.AssertAWSError(t, err, "InvalidKeyPair.Duplicate")
	})

	// 8k: ImportKeyPair re-using the Phase 3 primary key.
	t.Run("8k_ImportKeyPairDuplicate", func(t *testing.T) {
		t.Parallel()
		harness.Step(t, "import-key-pair %s (duplicate)", existingKey)
		_, err := fix.AWS.EC2.ImportKeyPair(&ec2.ImportKeyPairInput{
			KeyName:           aws.String(existingKey),
			PublicKeyMaterial: generateImportPubKey(t),
		})
		harness.AssertAWSError(t, err, "InvalidKeyPair.Duplicate")
	})

	// 8l: ImportKeyPair with garbage public-key material.
	t.Run("8l_InvalidKeyFormat", func(t *testing.T) {
		t.Parallel()
		const badKeyName = "bad-format-key"
		harness.Step(t, "import-key-pair %s (bad material)", badKeyName)
		_, err := fix.AWS.EC2.ImportKeyPair(&ec2.ImportKeyPairInput{
			KeyName:           aws.String(badKeyName),
			PublicKeyMaterial: []byte("not-a-valid-public-key"),
		})
		// Defensive: if the gateway erroneously accepted the key, remove it
		// before the next sub-test runs.
		if err == nil {
			t.Cleanup(func() {
				_, _ = fix.AWS.EC2.DeleteKeyPair(&ec2.DeleteKeyPairInput{
					KeyName: aws.String(badKeyName),
				})
			})
		}
		harness.AssertAWSError(t, err, "InvalidKey.Format")
	})

	// 8m: DescribeVolumes filtered by a non-existent volume id.
	t.Run("8m_DescribeVolumesNotFound", func(t *testing.T) {
		t.Parallel()
		harness.Step(t, "describe-volumes vol-0000000000000dead")
		_, err := fix.AWS.EC2.DescribeVolumes(&ec2.DescribeVolumesInput{
			VolumeIds: []*string{aws.String("vol-0000000000000dead")},
		})
		harness.AssertAWSError(t, err, "InvalidVolume.NotFound")
	})

	// 8n: DescribeImages filtered by a non-existent AMI id.
	t.Run("8n_DescribeImagesNotFound", func(t *testing.T) {
		t.Parallel()
		harness.Step(t, "describe-images ami-0000000000000dead")
		_, err := fix.AWS.EC2.DescribeImages(&ec2.DescribeImagesInput{
			ImageIds: []*string{aws.String("ami-0000000000000dead")},
		})
		harness.AssertAWSError(t, err, "InvalidAMIID.NotFound")
	})

	// 8o: CreateImage with a name that already exists (Phase 5e's AMI).
	// Stored in customAMIID, but the duplicate-name check is by name, not
	// id — assert via the well-known constant from Phase 5e.
	t.Run("8o_CreateImageDuplicateName", func(t *testing.T) {
		t.Parallel()
		harness.Step(t, "create-image name=%s (duplicate)", customAMIName)
		out, err := fix.AWS.EC2.CreateImage(&ec2.CreateImageInput{
			InstanceId: aws.String(instanceID),
			Name:       aws.String(customAMIName),
		})
		// If a second AMI somehow got minted, deregister it so it doesn't
		// linger past the suite.
		if err == nil && out != nil && aws.StringValue(out.ImageId) != "" &&
			aws.StringValue(out.ImageId) != customAMIID {
			extraID := aws.StringValue(out.ImageId)
			t.Cleanup(func() {
				_, _ = fix.AWS.EC2.DeregisterImage(&ec2.DeregisterImageInput{
					ImageId: aws.String(extraID),
				})
			})
		}
		harness.AssertAWSError(t, err, "InvalidAMIName.Duplicate")
	})

	// 8p: DeleteKeyPair on an unknown key must succeed (idempotent — matches
	// AWS). This is the only positive case in Phase 8.
	t.Run("8p_DeleteKeyPairIdempotent", func(t *testing.T) {
		t.Parallel()
		const missingKey = "nonexistent-key-99999"
		harness.Step(t, "delete-key-pair %s (idempotent)", missingKey)
		_, err := fix.AWS.EC2.DeleteKeyPair(&ec2.DeleteKeyPairInput{
			KeyName: aws.String(missingKey),
		})
		require.NoError(t, err, "delete-key-pair on missing key must succeed (idempotent)")
	})

	// 8q: ModifyInstanceAttribute on a running instance — gateway requires
	// the instance to be stopped, and the running instance isn't in the
	// stopped KV view, so the error surfaces as NotFound rather than
	// IncorrectInstanceState. Matches bash expectation.
	t.Run("8q_ModifyAttributeOnRunning", func(t *testing.T) {
		t.Parallel()
		harness.Step(t, "modify-instance-attribute %s (running)", instanceID)
		_, err := fix.AWS.EC2.ModifyInstanceAttribute(&ec2.ModifyInstanceAttributeInput{
			InstanceId: aws.String(instanceID),
			InstanceType: &ec2.AttributeValue{
				Value: aws.String(instType),
			},
		})
		harness.AssertAWSError(t, err, "InvalidInstanceID.NotFound")
	})

	// 8r: RebootInstances on a non-existent instance id.
	t.Run("8r_RebootInstanceNotFound", func(t *testing.T) {
		t.Parallel()
		harness.Step(t, "reboot-instances i-nonexistent")
		_, err := fix.AWS.EC2.RebootInstances(&ec2.RebootInstancesInput{
			InstanceIds: []*string{aws.String("i-nonexistent")},
		})
		harness.AssertAWSError(t, err, "InvalidInstanceID.NotFound")
	})
}

// cleanupUnexpectedInstances registers a t.Cleanup that terminates any
// instances the gateway accidentally launched from a negative RunInstances
// call. Most failures return nil reservations, so this is a no-op in the
// happy path. Errors during cleanup are swallowed — the sub-test has already
// failed if it got this far with non-nil out.
func cleanupUnexpectedInstances(t *testing.T, fix *Fixture, out *ec2.Reservation) {
	t.Helper()
	if out == nil || len(out.Instances) == 0 {
		return
	}
	ids := make([]*string, 0, len(out.Instances))
	for _, inst := range out.Instances {
		if id := aws.StringValue(inst.InstanceId); id != "" {
			ids = append(ids, aws.String(id))
		}
	}
	if len(ids) == 0 {
		return
	}
	t.Cleanup(func() {
		_, _ = fix.AWS.EC2.TerminateInstances(&ec2.TerminateInstancesInput{
			InstanceIds: ids,
		})
	})
}
