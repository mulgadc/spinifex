//go:build e2e

package single

import (
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/awserr"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/tests/e2e/harness"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// runAccountScoping ports run-e2e.sh Phase 8 EC2 Account Scoping
// (lines 1915–2624). Validates that EC2 resources (instances, volumes, key
// pairs, snapshots, VPCs, IGWs, EIGWs) are isolated between tenant accounts
// created via `spx admin account create`.
//
// Three admin accounts are created (Team Alpha, Team Beta, Team Gamma). All
// resources are launched through their scoped *AWSClient (statically-credentialed,
// bypasses AWS_PROFILE). A single t.Cleanup at the outer function tears down
// every resource across all three accounts regardless of which sub-step
// failed — every delete is best-effort / not-found tolerant. There is no
// `spx admin account delete` so the account rows themselves persist beyond
// the test; the carousel entry is local-only state and goes out of scope
// with the function.
//
// Sub-steps:
//
//	Step1_AccountSetup        — create Alpha + Beta, smoke-test auth
//	Step2_InstanceScoping     — instance describe + cross-account ops blocked
//	Step3_VolumeScoping       — volume describe + attach/detach/modify
//	Step4_KeyPairScoping      — key pair namespace + cross-account delete no-op
//	Step5_SnapshotScoping     — snapshot describe + OwnerId + delete
//	Step6_VPCSubnetScoping    — VPC/subnet describe + cross-account ops
//	Step7_IGWEIGWScoping      — IGW/EIGW describe + attach/detach
//	Step8_AccountSettings     — EBS-encryption-by-default per-tenant
//	Step9_GlobalResources     — regions/AZs/instance types identical
//	Step10_EdgeCases          — empty Gamma + non-existent resource ids
//	Step11_Cleanup            — explicit best-effort teardown (also runs via t.Cleanup)
func runAccountScoping(t *testing.T, fix *Fixture) {
	harness.Phase(t, "Single — EC2 Account Scoping")

	amiID := needAMI(t, fix)
	instType, _ := needInstanceTypeArch(t, fix)
	az := needAZ(t, fix)

	carousel := harness.NewAccountCarousel()

	// State threaded across Steps. The teardown below references whatever
	// is populated at failure time; zero values are skipped.
	var (
		alphaInst, betaInst        string
		alphaVol, betaVol          string
		alphaSnap, betaSnap        string
		alphaVPC, betaVPC          string
		alphaSubnet, betaSubnet    string
		alphaIGW, betaIGW          string
		alphaEIGW, betaEIGW        string
		alphaKeyNames              = []string{}
		betaKeyNames               = []string{}
		alphaEncryptionLeftEnabled bool
		betaEncryptionLeftEnabled  bool
	)

	// t.Cleanup runs LIFO regardless of which Step failed. Every call below
	// swallows not-found errors so a partial Step 11 run + this cleanup is
	// idempotent. Step 11's explicit cleanup is duplicated here intentionally:
	// the bash script's `2>/dev/null || true` pattern translates to "ignore
	// any error" in Go, which is exactly what these closures do.
	t.Cleanup(func() {
		harness.Step(t, "Phase 8 acct cleanup (deferred)")
		alpha := carousel.Get("spx-team-alpha")
		beta := carousel.Get("spx-team-beta")

		// Terminate instances.
		if alpha != nil && alphaInst != "" {
			_, _ = alpha.Client.EC2.TerminateInstances(&ec2.TerminateInstancesInput{
				InstanceIds: []*string{aws.String(alphaInst)},
			})
		}
		if beta != nil && betaInst != "" {
			_, _ = beta.Client.EC2.TerminateInstances(&ec2.TerminateInstancesInput{
				InstanceIds: []*string{aws.String(betaInst)},
			})
		}
		// Wait for termination so volumes / snapshots release cleanly.
		if alpha != nil && alphaInst != "" {
			waitTerminated(t, alpha.Client, alphaInst)
		}
		if beta != nil && betaInst != "" {
			waitTerminated(t, beta.Client, betaInst)
		}

		// Snapshots.
		if alpha != nil && alphaSnap != "" {
			_, _ = alpha.Client.EC2.DeleteSnapshot(&ec2.DeleteSnapshotInput{
				SnapshotId: aws.String(alphaSnap),
			})
		}
		if beta != nil && betaSnap != "" {
			_, _ = beta.Client.EC2.DeleteSnapshot(&ec2.DeleteSnapshotInput{
				SnapshotId: aws.String(betaSnap),
			})
		}

		// Volumes (give a moment for detach-on-terminate to settle).
		time.Sleep(3 * time.Second)
		if alpha != nil && alphaVol != "" {
			_, _ = alpha.Client.EC2.DeleteVolume(&ec2.DeleteVolumeInput{
				VolumeId: aws.String(alphaVol),
			})
		}
		if beta != nil && betaVol != "" {
			_, _ = beta.Client.EC2.DeleteVolume(&ec2.DeleteVolumeInput{
				VolumeId: aws.String(betaVol),
			})
		}

		// Key pairs.
		if alpha != nil {
			for _, k := range alphaKeyNames {
				_, _ = alpha.Client.EC2.DeleteKeyPair(&ec2.DeleteKeyPairInput{
					KeyName: aws.String(k),
				})
			}
		}
		if beta != nil {
			for _, k := range betaKeyNames {
				_, _ = beta.Client.EC2.DeleteKeyPair(&ec2.DeleteKeyPairInput{
					KeyName: aws.String(k),
				})
			}
		}

		// EIGWs.
		if alpha != nil && alphaEIGW != "" {
			_, _ = alpha.Client.EC2.DeleteEgressOnlyInternetGateway(&ec2.DeleteEgressOnlyInternetGatewayInput{
				EgressOnlyInternetGatewayId: aws.String(alphaEIGW),
			})
		}
		if beta != nil && betaEIGW != "" {
			_, _ = beta.Client.EC2.DeleteEgressOnlyInternetGateway(&ec2.DeleteEgressOnlyInternetGatewayInput{
				EgressOnlyInternetGatewayId: aws.String(betaEIGW),
			})
		}

		// IGWs (must detach before delete).
		if alpha != nil && alphaIGW != "" {
			if alphaVPC != "" {
				_, _ = alpha.Client.EC2.DetachInternetGateway(&ec2.DetachInternetGatewayInput{
					InternetGatewayId: aws.String(alphaIGW),
					VpcId:             aws.String(alphaVPC),
				})
			}
			_, _ = alpha.Client.EC2.DeleteInternetGateway(&ec2.DeleteInternetGatewayInput{
				InternetGatewayId: aws.String(alphaIGW),
			})
		}
		if beta != nil && betaIGW != "" {
			_, _ = beta.Client.EC2.DeleteInternetGateway(&ec2.DeleteInternetGatewayInput{
				InternetGatewayId: aws.String(betaIGW),
			})
		}

		// Subnets.
		if alpha != nil && alphaSubnet != "" {
			_, _ = alpha.Client.EC2.DeleteSubnet(&ec2.DeleteSubnetInput{
				SubnetId: aws.String(alphaSubnet),
			})
		}
		if beta != nil && betaSubnet != "" {
			_, _ = beta.Client.EC2.DeleteSubnet(&ec2.DeleteSubnetInput{
				SubnetId: aws.String(betaSubnet),
			})
		}

		// VPCs.
		if alpha != nil && alphaVPC != "" {
			_, _ = alpha.Client.EC2.DeleteVpc(&ec2.DeleteVpcInput{
				VpcId: aws.String(alphaVPC),
			})
		}
		if beta != nil && betaVPC != "" {
			_, _ = beta.Client.EC2.DeleteVpc(&ec2.DeleteVpcInput{
				VpcId: aws.String(betaVPC),
			})
		}

		// Reset EBS encryption defaults if Step 8 left them on.
		if alpha != nil && alphaEncryptionLeftEnabled {
			_, _ = alpha.Client.EC2.DisableEbsEncryptionByDefault(&ec2.DisableEbsEncryptionByDefaultInput{})
		}
		if beta != nil && betaEncryptionLeftEnabled {
			_, _ = beta.Client.EC2.DisableEbsEncryptionByDefault(&ec2.DisableEbsEncryptionByDefaultInput{})
		}
	})

	// ---------------------------------------------------------------------
	// Step 1: Account Setup (bash 1928–1965)
	// ---------------------------------------------------------------------
	t.Run("Step1_AccountSetup", func(t *testing.T) {
		harness.Step(t, "create account 'Team Alpha'")
		alphaInfo := harness.SpxAdminAccountCreate(t, "Team Alpha", "")
		require.NotEmpty(t, alphaInfo.AccountID, "alpha AccountID")
		require.NotEmpty(t, alphaInfo.AccessKeyID, "alpha AccessKeyID")
		alpha := carousel.Add(t, fix.Env, "spx-team-alpha", alphaInfo)
		harness.Detail(t, "alpha_account", alphaInfo.AccountID, "alpha_key", alphaInfo.AccessKeyID)

		harness.Step(t, "create account 'Team Beta'")
		betaInfo := harness.SpxAdminAccountCreate(t, "Team Beta", "")
		require.NotEmpty(t, betaInfo.AccountID, "beta AccountID")
		require.NotEmpty(t, betaInfo.AccessKeyID, "beta AccessKeyID")
		beta := carousel.Add(t, fix.Env, "spx-team-beta", betaInfo)
		harness.Detail(t, "beta_account", betaInfo.AccountID, "beta_key", betaInfo.AccessKeyID)

		// Auth smoke-test — bash just gates on exit 0.
		_, err := alpha.Client.EC2.DescribeInstances(&ec2.DescribeInstancesInput{})
		require.NoError(t, err, "alpha describe-instances (auth check)")
		_, err = beta.Client.EC2.DescribeInstances(&ec2.DescribeInstancesInput{})
		require.NoError(t, err, "beta describe-instances (auth check)")
	})

	// Bail out if Step 1 didn't seat both profiles — every subsequent step
	// derefs them. Step 11 still runs via t.Cleanup with whatever state exists.
	alpha := carousel.Get("spx-team-alpha")
	beta := carousel.Get("spx-team-beta")
	if alpha == nil || beta == nil {
		t.Fatalf("Step 1 did not register both alpha and beta carousel entries")
	}

	// ---------------------------------------------------------------------
	// Step 2: Instance Scoping (bash 1969–2088)
	// ---------------------------------------------------------------------
	t.Run("Step2_InstanceScoping", func(t *testing.T) {
		const alphaInstKey = "alpha-instance-key"
		const betaInstKey = "beta-instance-key"

		harness.Step(t, "create per-account key pairs")
		_, err := alpha.Client.EC2.CreateKeyPair(&ec2.CreateKeyPairInput{KeyName: aws.String(alphaInstKey)})
		require.NoError(t, err, "alpha create-key-pair")
		alphaKeyNames = append(alphaKeyNames, alphaInstKey)
		_, err = beta.Client.EC2.CreateKeyPair(&ec2.CreateKeyPairInput{KeyName: aws.String(betaInstKey)})
		require.NoError(t, err, "beta create-key-pair")
		betaKeyNames = append(betaKeyNames, betaInstKey)

		harness.Step(t, "alpha run-instances")
		alphaRun, err := alpha.Client.EC2.RunInstances(&ec2.RunInstancesInput{
			ImageId:      aws.String(amiID),
			InstanceType: aws.String(instType),
			KeyName:      aws.String(alphaInstKey),
			MinCount:     aws.Int64(1),
			MaxCount:     aws.Int64(1),
		})
		require.NoError(t, err, "alpha run-instances")
		require.NotEmpty(t, alphaRun.Instances, "alpha run-instances: no instances")
		alphaInst = aws.StringValue(alphaRun.Instances[0].InstanceId)
		require.NotEmpty(t, alphaInst, "alpha InstanceId empty")
		harness.Detail(t, "alpha_instance", alphaInst)

		harness.Step(t, "beta run-instances")
		betaRun, err := beta.Client.EC2.RunInstances(&ec2.RunInstancesInput{
			ImageId:      aws.String(amiID),
			InstanceType: aws.String(instType),
			KeyName:      aws.String(betaInstKey),
			MinCount:     aws.Int64(1),
			MaxCount:     aws.Int64(1),
		})
		require.NoError(t, err, "beta run-instances")
		require.NotEmpty(t, betaRun.Instances, "beta run-instances: no instances")
		betaInst = aws.StringValue(betaRun.Instances[0].InstanceId)
		require.NotEmpty(t, betaInst, "beta InstanceId empty")
		harness.Detail(t, "beta_instance", betaInst)

		harness.WaitForInstanceState(t, alpha.Client, alphaInst, "running")
		harness.WaitForInstanceState(t, beta.Client, betaInst, "running")

		harness.Step(t, "alpha sees only own instances")
		alphaIDs := describeInstanceIDs(t, alpha.Client)
		assert.NotContains(t, alphaIDs, betaInst, "alpha saw beta's instance")

		harness.Step(t, "beta sees only own instances")
		betaIDs := describeInstanceIDs(t, beta.Client)
		assert.NotContains(t, betaIDs, alphaInst, "beta saw alpha's instance")

		harness.Step(t, "alpha OwnerId matches alpha account")
		alphaDesc, err := alpha.Client.EC2.DescribeInstances(&ec2.DescribeInstancesInput{})
		require.NoError(t, err, "alpha describe-instances")
		require.NotEmpty(t, alphaDesc.Reservations, "alpha reservations empty")
		assert.Equal(t, alpha.AccountID, aws.StringValue(alphaDesc.Reservations[0].OwnerId),
			"alpha OwnerId mismatch")

		harness.Step(t, "cross-account stop alpha->beta blocked")
		harness.ExpectError(t, "InvalidInstanceID.NotFound", func() error {
			_, err := alpha.Client.EC2.StopInstances(&ec2.StopInstancesInput{
				InstanceIds: []*string{aws.String(betaInst)},
			})
			return err
		})

		harness.Step(t, "cross-account terminate beta->alpha blocked")
		harness.ExpectError(t, "InvalidInstanceID.NotFound", func() error {
			_, err := beta.Client.EC2.TerminateInstances(&ec2.TerminateInstancesInput{
				InstanceIds: []*string{aws.String(alphaInst)},
			})
			return err
		})

		harness.Step(t, "cross-account reboot alpha->beta blocked")
		harness.ExpectError(t, "InvalidInstanceID.NotFound", func() error {
			_, err := alpha.Client.EC2.RebootInstances(&ec2.RebootInstancesInput{
				InstanceIds: []*string{aws.String(betaInst)},
			})
			return err
		})

		// Stop alpha for cross-account start/modify/console assertions.
		harness.Step(t, "stop alpha instance for cross-account start/modify/console tests")
		_, err = alpha.Client.EC2.StopInstances(&ec2.StopInstancesInput{
			InstanceIds: []*string{aws.String(alphaInst)},
		})
		require.NoError(t, err, "alpha stop-instances")
		harness.WaitForInstanceState(t, alpha.Client, alphaInst, "stopped")

		harness.Step(t, "cross-account start beta->alpha blocked")
		harness.ExpectError(t, "InvalidInstanceID.NotFound", func() error {
			_, err := beta.Client.EC2.StartInstances(&ec2.StartInstancesInput{
				InstanceIds: []*string{aws.String(alphaInst)},
			})
			return err
		})

		harness.Step(t, "cross-account modify-instance-attribute beta->alpha blocked")
		harness.ExpectError(t, "InvalidInstanceID.NotFound", func() error {
			_, err := beta.Client.EC2.ModifyInstanceAttribute(&ec2.ModifyInstanceAttributeInput{
				InstanceId:   aws.String(alphaInst),
				InstanceType: &ec2.AttributeValue{Value: aws.String("t2.small")},
			})
			return err
		})

		harness.Step(t, "cross-account get-console-output beta->alpha blocked")
		harness.ExpectError(t, "InvalidInstanceID.NotFound", func() error {
			_, err := beta.Client.EC2.GetConsoleOutput(&ec2.GetConsoleOutputInput{
				InstanceId: aws.String(alphaInst),
			})
			return err
		})

		// Restart alpha for Step 3 (attach test needs it running).
		harness.Step(t, "restart alpha instance")
		_, err = alpha.Client.EC2.StartInstances(&ec2.StartInstancesInput{
			InstanceIds: []*string{aws.String(alphaInst)},
		})
		require.NoError(t, err, "alpha start-instances")
		harness.WaitForInstanceState(t, alpha.Client, alphaInst, "running")
	})

	// ---------------------------------------------------------------------
	// Step 3: Volume Scoping (bash 2092–2146)
	// ---------------------------------------------------------------------
	t.Run("Step3_VolumeScoping", func(t *testing.T) {
		harness.Step(t, "alpha create-volume")
		av, err := alpha.Client.EC2.CreateVolume(&ec2.CreateVolumeInput{
			AvailabilityZone: aws.String(az),
			Size:             aws.Int64(10),
			VolumeType:       aws.String("gp3"),
		})
		require.NoError(t, err, "alpha create-volume")
		alphaVol = aws.StringValue(av.VolumeId)
		require.NotEmpty(t, alphaVol)
		harness.Detail(t, "alpha_vol", alphaVol)

		harness.Step(t, "beta create-volume")
		bv, err := beta.Client.EC2.CreateVolume(&ec2.CreateVolumeInput{
			AvailabilityZone: aws.String(az),
			Size:             aws.Int64(10),
			VolumeType:       aws.String("gp3"),
		})
		require.NoError(t, err, "beta create-volume")
		betaVol = aws.StringValue(bv.VolumeId)
		require.NotEmpty(t, betaVol)
		harness.Detail(t, "beta_vol", betaVol)

		harness.WaitForVolumeState(t, alpha.Client, alphaVol, "available")
		harness.WaitForVolumeState(t, beta.Client, betaVol, "available")

		harness.Step(t, "alpha sees only own volumes")
		alphaVols := describeVolumeIDs(t, alpha.Client)
		assert.NotContains(t, alphaVols, betaVol, "alpha saw beta's volume")

		harness.Step(t, "cross-account describe-volume by id blocked")
		harness.ExpectError(t, "InvalidVolume.NotFound", func() error {
			_, err := alpha.Client.EC2.DescribeVolumes(&ec2.DescribeVolumesInput{
				VolumeIds: []*string{aws.String(betaVol)},
			})
			return err
		})

		harness.Step(t, "cross-account delete-volume blocked")
		harness.ExpectError(t, "InvalidVolume.NotFound", func() error {
			_, err := beta.Client.EC2.DeleteVolume(&ec2.DeleteVolumeInput{
				VolumeId: aws.String(alphaVol),
			})
			return err
		})

		harness.Step(t, "cross-account attach-volume (beta attaches alpha's vol) blocked")
		harness.ExpectError(t, "InvalidVolume.NotFound", func() error {
			_, err := beta.Client.EC2.AttachVolume(&ec2.AttachVolumeInput{
				VolumeId:   aws.String(alphaVol),
				InstanceId: aws.String(betaInst),
				Device:     aws.String("/dev/sdf"),
			})
			return err
		})

		// Attach alpha's volume to alpha's instance, then test cross-account detach.
		harness.Step(t, "alpha attach-volume to own instance")
		_, err = alpha.Client.EC2.AttachVolume(&ec2.AttachVolumeInput{
			VolumeId:   aws.String(alphaVol),
			InstanceId: aws.String(alphaInst),
			Device:     aws.String("/dev/sdf"),
		})
		require.NoError(t, err, "alpha attach-volume")
		harness.WaitForVolumeState(t, alpha.Client, alphaVol, "in-use")

		harness.Step(t, "cross-account detach-volume blocked")
		harness.ExpectError(t, "InvalidVolume.NotFound", func() error {
			_, err := beta.Client.EC2.DetachVolume(&ec2.DetachVolumeInput{
				VolumeId: aws.String(alphaVol),
			})
			return err
		})

		harness.Step(t, "cross-account modify-volume blocked")
		harness.ExpectError(t, "InvalidVolume.NotFound", func() error {
			_, err := beta.Client.EC2.ModifyVolume(&ec2.ModifyVolumeInput{
				VolumeId: aws.String(alphaVol),
				Size:     aws.Int64(20),
			})
			return err
		})

		// Detach so Step 11 / cleanup can delete the volume cleanly.
		harness.Step(t, "alpha detach own volume")
		_, err = alpha.Client.EC2.DetachVolume(&ec2.DetachVolumeInput{
			VolumeId: aws.String(alphaVol),
		})
		require.NoError(t, err, "alpha detach-volume")
		harness.WaitForVolumeState(t, alpha.Client, alphaVol, "available")
	})

	// ---------------------------------------------------------------------
	// Step 4: Key Pair Scoping (bash 2150–2206)
	// ---------------------------------------------------------------------
	t.Run("Step4_KeyPairScoping", func(t *testing.T) {
		const alphaKey = "alpha-key"
		const betaKey = "beta-key"
		const sharedKey = "shared-name"
		const importedKey = "imported-key"

		harness.Step(t, "alpha create-key-pair %s", alphaKey)
		_, err := alpha.Client.EC2.CreateKeyPair(&ec2.CreateKeyPairInput{KeyName: aws.String(alphaKey)})
		require.NoError(t, err, "alpha create-key-pair %s", alphaKey)
		alphaKeyNames = append(alphaKeyNames, alphaKey)
		alphaKeyID := describeKeyPairID(t, alpha.Client, alphaKey)
		require.NotEmpty(t, alphaKeyID, "alpha key-pair id")

		harness.Step(t, "beta create-key-pair %s", betaKey)
		_, err = beta.Client.EC2.CreateKeyPair(&ec2.CreateKeyPairInput{KeyName: aws.String(betaKey)})
		require.NoError(t, err, "beta create-key-pair %s", betaKey)
		betaKeyNames = append(betaKeyNames, betaKey)

		harness.Step(t, "alpha sees only own keys")
		alphaKeys := describeKeyPairNames(t, alpha.Client)
		assert.NotContains(t, alphaKeys, betaKey, "alpha saw beta's key")

		// Same name in both accounts — different KeyPairIds.
		harness.Step(t, "namespace isolation: %s in both accounts", sharedKey)
		_, err = alpha.Client.EC2.CreateKeyPair(&ec2.CreateKeyPairInput{KeyName: aws.String(sharedKey)})
		require.NoError(t, err, "alpha create-key-pair %s", sharedKey)
		alphaKeyNames = append(alphaKeyNames, sharedKey)
		_, err = beta.Client.EC2.CreateKeyPair(&ec2.CreateKeyPairInput{KeyName: aws.String(sharedKey)})
		require.NoError(t, err, "beta create-key-pair %s", sharedKey)
		betaKeyNames = append(betaKeyNames, sharedKey)
		alphaShared := describeKeyPairID(t, alpha.Client, sharedKey)
		betaShared := describeKeyPairID(t, beta.Client, sharedKey)
		require.NotEmpty(t, alphaShared, "alpha shared key id")
		require.NotEmpty(t, betaShared, "beta shared key id")
		assert.NotEqual(t, alphaShared, betaShared, "same KeyPairId across accounts")

		// Cross-account delete is idempotent (no error) but must not affect the other account.
		harness.Step(t, "beta deletes alpha-key (idempotent, no cross-account effect)")
		_, _ = beta.Client.EC2.DeleteKeyPair(&ec2.DeleteKeyPairInput{KeyName: aws.String(alphaKey)})
		assert.Equal(t, alphaKeyID, describeKeyPairID(t, alpha.Client, alphaKey),
			"beta's delete affected alpha's key")

		// Import key pair is also account-scoped.
		harness.Step(t, "alpha import-key-pair %s", importedKey)
		_, err = alpha.Client.EC2.ImportKeyPair(&ec2.ImportKeyPairInput{
			KeyName:           aws.String(importedKey),
			PublicKeyMaterial: generateImportPubKey(t),
		})
		require.NoError(t, err, "alpha import-key-pair")
		alphaKeyNames = append(alphaKeyNames, importedKey)

		betaKeys := describeKeyPairNames(t, beta.Client)
		assert.NotContains(t, betaKeys, importedKey, "beta saw alpha's imported key")
	})

	// ---------------------------------------------------------------------
	// Step 5: Snapshot Scoping (bash 2210–2250)
	// ---------------------------------------------------------------------
	t.Run("Step5_SnapshotScoping", func(t *testing.T) {
		require.NotEmpty(t, alphaVol, "Step 3 must populate alphaVol")
		require.NotEmpty(t, betaVol, "Step 3 must populate betaVol")

		harness.Step(t, "alpha create-snapshot")
		as, err := alpha.Client.EC2.CreateSnapshot(&ec2.CreateSnapshotInput{
			VolumeId:    aws.String(alphaVol),
			Description: aws.String("Alpha snapshot"),
		})
		require.NoError(t, err, "alpha create-snapshot")
		alphaSnap = aws.StringValue(as.SnapshotId)
		require.NotEmpty(t, alphaSnap)
		harness.Detail(t, "alpha_snap", alphaSnap)

		harness.Step(t, "beta create-snapshot")
		bs, err := beta.Client.EC2.CreateSnapshot(&ec2.CreateSnapshotInput{
			VolumeId:    aws.String(betaVol),
			Description: aws.String("Beta snapshot"),
		})
		require.NoError(t, err, "beta create-snapshot")
		betaSnap = aws.StringValue(bs.SnapshotId)
		require.NotEmpty(t, betaSnap)
		harness.Detail(t, "beta_snap", betaSnap)

		harness.Step(t, "alpha sees only own snapshots (owner-ids=self)")
		alphaSnaps, err := alpha.Client.EC2.DescribeSnapshots(&ec2.DescribeSnapshotsInput{
			OwnerIds: []*string{aws.String("self")},
		})
		require.NoError(t, err, "alpha describe-snapshots")
		var alphaIDs []string
		for _, s := range alphaSnaps.Snapshots {
			alphaIDs = append(alphaIDs, aws.StringValue(s.SnapshotId))
		}
		assert.NotContains(t, alphaIDs, betaSnap, "alpha saw beta's snapshot")

		// OwnerId verification.
		require.NotEmpty(t, alphaSnaps.Snapshots, "alpha snapshots empty")
		assert.Equal(t, alpha.AccountID, aws.StringValue(alphaSnaps.Snapshots[0].OwnerId),
			"alpha snapshot OwnerId mismatch")

		// Cross-account delete — bash expects UnauthorizedOperation (NOT the
		// usual NotFound). This is a Snapshot-specific gateway behavior.
		harness.Step(t, "cross-account delete-snapshot blocked (UnauthorizedOperation)")
		harness.ExpectError(t, "UnauthorizedOperation", func() error {
			_, err := beta.Client.EC2.DeleteSnapshot(&ec2.DeleteSnapshotInput{
				SnapshotId: aws.String(alphaSnap),
			})
			return err
		})

		harness.Step(t, "cross-account create-snapshot of alpha's volume blocked")
		harness.ExpectError(t, "InvalidVolume.NotFound", func() error {
			_, err := beta.Client.EC2.CreateSnapshot(&ec2.CreateSnapshotInput{
				VolumeId:    aws.String(alphaVol),
				Description: aws.String("stolen"),
			})
			return err
		})
	})

	// ---------------------------------------------------------------------
	// Step 6: VPC/Subnet Scoping (bash 2254–2313)
	// ---------------------------------------------------------------------
	t.Run("Step6_VPCSubnetScoping", func(t *testing.T) {
		harness.Step(t, "alpha create-vpc 10.0.0.0/16")
		av, err := alpha.Client.EC2.CreateVpc(&ec2.CreateVpcInput{
			CidrBlock: aws.String("10.0.0.0/16"),
		})
		require.NoError(t, err, "alpha create-vpc")
		alphaVPC = aws.StringValue(av.Vpc.VpcId)
		require.NotEmpty(t, alphaVPC)
		harness.Detail(t, "alpha_vpc", alphaVPC)

		harness.Step(t, "beta create-vpc 10.0.0.0/16 (same CIDR, no conflict)")
		bv, err := beta.Client.EC2.CreateVpc(&ec2.CreateVpcInput{
			CidrBlock: aws.String("10.0.0.0/16"),
		})
		require.NoError(t, err, "beta create-vpc")
		betaVPC = aws.StringValue(bv.Vpc.VpcId)
		require.NotEmpty(t, betaVPC)
		harness.Detail(t, "beta_vpc", betaVPC)

		harness.Step(t, "alpha describe-vpcs isolation")
		alphaVPCs, err := alpha.Client.EC2.DescribeVpcs(&ec2.DescribeVpcsInput{})
		require.NoError(t, err, "alpha describe-vpcs")
		assert.NotContains(t, vpcIDs(alphaVPCs.Vpcs), betaVPC, "alpha saw beta's VPC")

		harness.Step(t, "cross-account describe-vpc by id blocked")
		harness.ExpectError(t, "InvalidVpcID.NotFound", func() error {
			_, err := alpha.Client.EC2.DescribeVpcs(&ec2.DescribeVpcsInput{
				VpcIds: []*string{aws.String(betaVPC)},
			})
			return err
		})

		harness.Step(t, "cross-account delete-vpc blocked")
		harness.ExpectError(t, "InvalidVpcID.NotFound", func() error {
			_, err := beta.Client.EC2.DeleteVpc(&ec2.DeleteVpcInput{
				VpcId: aws.String(alphaVPC),
			})
			return err
		})

		harness.Step(t, "alpha create-subnet 10.0.1.0/24")
		as, err := alpha.Client.EC2.CreateSubnet(&ec2.CreateSubnetInput{
			VpcId:     aws.String(alphaVPC),
			CidrBlock: aws.String("10.0.1.0/24"),
		})
		require.NoError(t, err, "alpha create-subnet")
		alphaSubnet = aws.StringValue(as.Subnet.SubnetId)
		require.NotEmpty(t, alphaSubnet)

		harness.Step(t, "beta create-subnet 10.0.1.0/24")
		bs, err := beta.Client.EC2.CreateSubnet(&ec2.CreateSubnetInput{
			VpcId:     aws.String(betaVPC),
			CidrBlock: aws.String("10.0.1.0/24"),
		})
		require.NoError(t, err, "beta create-subnet")
		betaSubnet = aws.StringValue(bs.Subnet.SubnetId)
		require.NotEmpty(t, betaSubnet)

		harness.Step(t, "alpha describe-subnets isolation")
		alphaSubnets, err := alpha.Client.EC2.DescribeSubnets(&ec2.DescribeSubnetsInput{})
		require.NoError(t, err, "alpha describe-subnets")
		var alphaSubnetIDs []string
		for _, s := range alphaSubnets.Subnets {
			alphaSubnetIDs = append(alphaSubnetIDs, aws.StringValue(s.SubnetId))
		}
		assert.NotContains(t, alphaSubnetIDs, betaSubnet, "alpha saw beta's subnet")

		harness.Step(t, "cross-account create-subnet in other VPC blocked")
		harness.ExpectError(t, "InvalidVpcID.NotFound", func() error {
			_, err := beta.Client.EC2.CreateSubnet(&ec2.CreateSubnetInput{
				VpcId:     aws.String(alphaVPC),
				CidrBlock: aws.String("10.0.2.0/24"),
			})
			return err
		})

		harness.Step(t, "cross-account delete-subnet blocked")
		harness.ExpectError(t, "InvalidSubnetID.NotFound", func() error {
			_, err := beta.Client.EC2.DeleteSubnet(&ec2.DeleteSubnetInput{
				SubnetId: aws.String(alphaSubnet),
			})
			return err
		})
	})

	// ---------------------------------------------------------------------
	// Step 7: IGW + EIGW Scoping (bash 2317–2403)
	// ---------------------------------------------------------------------
	t.Run("Step7_IGWEIGWScoping", func(t *testing.T) {
		require.NotEmpty(t, alphaVPC, "Step 6 must populate alphaVPC")
		require.NotEmpty(t, betaVPC, "Step 6 must populate betaVPC")

		harness.Step(t, "alpha create-internet-gateway")
		ai, err := alpha.Client.EC2.CreateInternetGateway(&ec2.CreateInternetGatewayInput{})
		require.NoError(t, err, "alpha create-internet-gateway")
		alphaIGW = aws.StringValue(ai.InternetGateway.InternetGatewayId)
		require.NotEmpty(t, alphaIGW)
		harness.Detail(t, "alpha_igw", alphaIGW)

		harness.Step(t, "beta create-internet-gateway")
		bi, err := beta.Client.EC2.CreateInternetGateway(&ec2.CreateInternetGatewayInput{})
		require.NoError(t, err, "beta create-internet-gateway")
		betaIGW = aws.StringValue(bi.InternetGateway.InternetGatewayId)
		require.NotEmpty(t, betaIGW)
		harness.Detail(t, "beta_igw", betaIGW)

		harness.Step(t, "alpha describe-internet-gateways isolation")
		alphaIGWs, err := alpha.Client.EC2.DescribeInternetGateways(&ec2.DescribeInternetGatewaysInput{})
		require.NoError(t, err, "alpha describe-internet-gateways")
		var alphaIGWIDs []string
		for _, ig := range alphaIGWs.InternetGateways {
			alphaIGWIDs = append(alphaIGWIDs, aws.StringValue(ig.InternetGatewayId))
		}
		assert.NotContains(t, alphaIGWIDs, betaIGW, "alpha saw beta's IGW")

		harness.Step(t, "cross-account describe IGW by id blocked")
		harness.ExpectError(t, "InvalidInternetGatewayID.NotFound", func() error {
			_, err := alpha.Client.EC2.DescribeInternetGateways(&ec2.DescribeInternetGatewaysInput{
				InternetGatewayIds: []*string{aws.String(betaIGW)},
			})
			return err
		})

		harness.Step(t, "cross-account delete IGW blocked")
		harness.ExpectError(t, "InvalidInternetGatewayID.NotFound", func() error {
			_, err := beta.Client.EC2.DeleteInternetGateway(&ec2.DeleteInternetGatewayInput{
				InternetGatewayId: aws.String(alphaIGW),
			})
			return err
		})

		harness.Step(t, "cross-account attach IGW (alpha attaches beta's IGW) blocked")
		harness.ExpectError(t, "InvalidInternetGatewayID.NotFound", func() error {
			_, err := alpha.Client.EC2.AttachInternetGateway(&ec2.AttachInternetGatewayInput{
				InternetGatewayId: aws.String(betaIGW),
				VpcId:             aws.String(alphaVPC),
			})
			return err
		})

		// Attach alpha's IGW to alpha's VPC, then verify beta can't detach.
		harness.Step(t, "alpha attach own IGW to own VPC")
		_, err = alpha.Client.EC2.AttachInternetGateway(&ec2.AttachInternetGatewayInput{
			InternetGatewayId: aws.String(alphaIGW),
			VpcId:             aws.String(alphaVPC),
		})
		require.NoError(t, err, "alpha attach-internet-gateway")

		harness.Step(t, "cross-account detach IGW blocked")
		harness.ExpectError(t, "InvalidInternetGatewayID.NotFound", func() error {
			_, err := beta.Client.EC2.DetachInternetGateway(&ec2.DetachInternetGatewayInput{
				InternetGatewayId: aws.String(alphaIGW),
				VpcId:             aws.String(alphaVPC),
			})
			return err
		})

		// EIGW
		harness.Step(t, "alpha create-egress-only-internet-gateway")
		ae, err := alpha.Client.EC2.CreateEgressOnlyInternetGateway(&ec2.CreateEgressOnlyInternetGatewayInput{
			VpcId: aws.String(alphaVPC),
		})
		require.NoError(t, err, "alpha create-eigw")
		alphaEIGW = aws.StringValue(ae.EgressOnlyInternetGateway.EgressOnlyInternetGatewayId)
		require.NotEmpty(t, alphaEIGW)
		harness.Detail(t, "alpha_eigw", alphaEIGW)

		harness.Step(t, "beta create-egress-only-internet-gateway")
		be, err := beta.Client.EC2.CreateEgressOnlyInternetGateway(&ec2.CreateEgressOnlyInternetGatewayInput{
			VpcId: aws.String(betaVPC),
		})
		require.NoError(t, err, "beta create-eigw")
		betaEIGW = aws.StringValue(be.EgressOnlyInternetGateway.EgressOnlyInternetGatewayId)
		require.NotEmpty(t, betaEIGW)
		harness.Detail(t, "beta_eigw", betaEIGW)

		harness.Step(t, "alpha describe-eigws isolation")
		alphaEIGWs, err := alpha.Client.EC2.DescribeEgressOnlyInternetGateways(&ec2.DescribeEgressOnlyInternetGatewaysInput{})
		require.NoError(t, err, "alpha describe-eigws")
		var alphaEIGWIDs []string
		for _, ig := range alphaEIGWs.EgressOnlyInternetGateways {
			alphaEIGWIDs = append(alphaEIGWIDs, aws.StringValue(ig.EgressOnlyInternetGatewayId))
		}
		assert.NotContains(t, alphaEIGWIDs, betaEIGW, "alpha saw beta's EIGW")

		// Cross-account EIGW delete — bash uses `expect_error "" ... || true`
		// (any error or none is acceptable). The hard assertion is that
		// alpha's EIGW survives.
		_, _ = beta.Client.EC2.DeleteEgressOnlyInternetGateway(&ec2.DeleteEgressOnlyInternetGatewayInput{
			EgressOnlyInternetGatewayId: aws.String(alphaEIGW),
		})
		check, err := alpha.Client.EC2.DescribeEgressOnlyInternetGateways(&ec2.DescribeEgressOnlyInternetGatewaysInput{})
		require.NoError(t, err, "alpha describe-eigws (post cross-account delete attempt)")
		var stillThere bool
		for _, ig := range check.EgressOnlyInternetGateways {
			if aws.StringValue(ig.EgressOnlyInternetGatewayId) == alphaEIGW {
				stillThere = true
				break
			}
		}
		assert.True(t, stillThere, "alpha's EIGW was deleted by beta")

		// Cross-account EIGW creation in other's VPC — bash also accepts any
		// error or none; the existence of alpha's EIGW is the contract.
		_, _ = beta.Client.EC2.CreateEgressOnlyInternetGateway(&ec2.CreateEgressOnlyInternetGatewayInput{
			VpcId: aws.String(alphaVPC),
		})
	})

	// ---------------------------------------------------------------------
	// Step 8: Account Settings (bash 2407–2435)
	// ---------------------------------------------------------------------
	t.Run("Step8_AccountSettings", func(t *testing.T) {
		harness.Step(t, "alpha enable-ebs-encryption-by-default")
		_, err := alpha.Client.EC2.EnableEbsEncryptionByDefault(&ec2.EnableEbsEncryptionByDefaultInput{})
		require.NoError(t, err, "alpha enable-ebs-encryption")
		alphaEncryptionLeftEnabled = true

		betaEnc, err := beta.Client.EC2.GetEbsEncryptionByDefault(&ec2.GetEbsEncryptionByDefaultInput{})
		require.NoError(t, err, "beta get-ebs-encryption")
		assert.False(t, aws.BoolValue(betaEnc.EbsEncryptionByDefault),
			"alpha's encryption setting leaked to beta")

		// Independent toggle: enable beta, disable alpha.
		_, err = beta.Client.EC2.EnableEbsEncryptionByDefault(&ec2.EnableEbsEncryptionByDefaultInput{})
		require.NoError(t, err, "beta enable-ebs-encryption")
		betaEncryptionLeftEnabled = true
		_, err = alpha.Client.EC2.DisableEbsEncryptionByDefault(&ec2.DisableEbsEncryptionByDefaultInput{})
		require.NoError(t, err, "alpha disable-ebs-encryption")
		alphaEncryptionLeftEnabled = false

		alphaEnc, err := alpha.Client.EC2.GetEbsEncryptionByDefault(&ec2.GetEbsEncryptionByDefaultInput{})
		require.NoError(t, err, "alpha get-ebs-encryption")
		betaEnc, err = beta.Client.EC2.GetEbsEncryptionByDefault(&ec2.GetEbsEncryptionByDefaultInput{})
		require.NoError(t, err, "beta get-ebs-encryption")
		assert.False(t, aws.BoolValue(alphaEnc.EbsEncryptionByDefault), "alpha encryption should be off")
		assert.True(t, aws.BoolValue(betaEnc.EbsEncryptionByDefault), "beta encryption should be on")

		// Reset beta — bash explicitly disables before moving on.
		_, err = beta.Client.EC2.DisableEbsEncryptionByDefault(&ec2.DisableEbsEncryptionByDefaultInput{})
		require.NoError(t, err, "beta disable-ebs-encryption (reset)")
		betaEncryptionLeftEnabled = false
	})

	// ---------------------------------------------------------------------
	// Step 9: Global Resources (bash 2439–2472)
	// ---------------------------------------------------------------------
	t.Run("Step9_GlobalResources", func(t *testing.T) {
		harness.Step(t, "describe-regions identical across accounts")
		alphaRegions := describeRegionNames(t, alpha.Client)
		betaRegions := describeRegionNames(t, beta.Client)
		assert.Equal(t, alphaRegions, betaRegions, "regions differ between accounts")

		harness.Step(t, "describe-availability-zones identical across accounts")
		alphaAZs := describeAZNames(t, alpha.Client)
		betaAZs := describeAZNames(t, beta.Client)
		assert.Equal(t, alphaAZs, betaAZs, "AZs differ between accounts")

		harness.Step(t, "describe-instance-types identical across accounts")
		alphaTypes := describeInstanceTypeNames(t, alpha.Client)
		betaTypes := describeInstanceTypeNames(t, beta.Client)
		assert.Equal(t, alphaTypes, betaTypes, "instance types differ between accounts")
	})

	// ---------------------------------------------------------------------
	// Step 10: Edge Cases (bash 2476–2542)
	// ---------------------------------------------------------------------
	t.Run("Step10_EdgeCases", func(t *testing.T) {
		harness.Step(t, "create empty Gamma account")
		gammaInfo := harness.SpxAdminAccountCreate(t, "Team Gamma", "")
		gamma := carousel.Add(t, fix.Env, "spx-team-gamma", gammaInfo)
		harness.Detail(t, "gamma_account", gammaInfo.AccountID)

		gInsts, err := gamma.Client.EC2.DescribeInstances(&ec2.DescribeInstancesInput{})
		require.NoError(t, err, "gamma describe-instances")
		var gInstIDs []string
		for _, r := range gInsts.Reservations {
			for _, i := range r.Instances {
				gInstIDs = append(gInstIDs, aws.StringValue(i.InstanceId))
			}
		}
		assert.Empty(t, gInstIDs, "gamma has instances")

		// Volume check is skipped — bash comments "root-account volumes
		// (empty TenantID) are visible to all accounts by design".

		gKeys := describeKeyPairNames(t, gamma.Client)
		assert.Empty(t, gKeys, "gamma has key pairs")

		gSnaps, err := gamma.Client.EC2.DescribeSnapshots(&ec2.DescribeSnapshotsInput{
			OwnerIds: []*string{aws.String("self")},
		})
		require.NoError(t, err, "gamma describe-snapshots")
		assert.Empty(t, gSnaps.Snapshots, "gamma has snapshots")

		// Root isolation: a key pair created on the root profile must not
		// appear under alpha. Also verify root cannot see alpha's instance.
		const rootKey = "root-scoping-key"
		harness.Step(t, "root create-key-pair %s", rootKey)
		_, err = fix.AWS.EC2.CreateKeyPair(&ec2.CreateKeyPairInput{KeyName: aws.String(rootKey)})
		require.NoError(t, err, "root create-key-pair %s", rootKey)
		t.Cleanup(func() {
			_, _ = fix.AWS.EC2.DeleteKeyPair(&ec2.DeleteKeyPairInput{KeyName: aws.String(rootKey)})
		})

		alphaKeys := describeKeyPairNames(t, alpha.Client)
		assert.NotContains(t, alphaKeys, rootKey, "alpha saw root's key pair")

		rootInsts := describeInstanceIDs(t, fix.AWS)
		assert.NotContains(t, rootInsts, alphaInst, "root saw tenant instance")

		_, _ = fix.AWS.EC2.DeleteKeyPair(&ec2.DeleteKeyPairInput{KeyName: aws.String(rootKey)})

		// Non-existent ids — same error code as cross-account.
		harness.Step(t, "non-existent volume id -> InvalidVolume.NotFound")
		harness.ExpectError(t, "InvalidVolume.NotFound", func() error {
			_, err := alpha.Client.EC2.DeleteVolume(&ec2.DeleteVolumeInput{
				VolumeId: aws.String("vol-00000000000000000"),
			})
			return err
		})

		harness.Step(t, "non-existent snapshot id -> InvalidSnapshot.NotFound")
		harness.ExpectError(t, "InvalidSnapshot.NotFound", func() error {
			_, err := alpha.Client.EC2.DeleteSnapshot(&ec2.DeleteSnapshotInput{
				SnapshotId: aws.String("snap-00000000000000000"),
			})
			return err
		})
	})

	// ---------------------------------------------------------------------
	// Step 11: EC2 Account Scoping Cleanup (bash 2546–2624)
	// ---------------------------------------------------------------------
	// Bash performs an explicit ordered teardown here. We do the same — but
	// the outer t.Cleanup also runs (idempotently) so a Step 11 failure
	// won't leak. Keeping Step 11 in-line preserves the bash structure and
	// the explicit assertions a triager expects when reading the log.
	t.Run("Step11_Cleanup", func(t *testing.T) {
		harness.Step(t, "terminate alpha instance")
		if alphaInst != "" {
			_, _ = alpha.Client.EC2.TerminateInstances(&ec2.TerminateInstancesInput{
				InstanceIds: []*string{aws.String(alphaInst)},
			})
		}
		harness.Step(t, "terminate beta instance")
		if betaInst != "" {
			_, _ = beta.Client.EC2.TerminateInstances(&ec2.TerminateInstancesInput{
				InstanceIds: []*string{aws.String(betaInst)},
			})
		}
		if alphaInst != "" {
			waitTerminated(t, alpha.Client, alphaInst)
		}
		if betaInst != "" {
			waitTerminated(t, beta.Client, betaInst)
		}

		harness.Step(t, "delete snapshots")
		if alphaSnap != "" {
			_, _ = alpha.Client.EC2.DeleteSnapshot(&ec2.DeleteSnapshotInput{
				SnapshotId: aws.String(alphaSnap),
			})
			alphaSnap = ""
		}
		if betaSnap != "" {
			_, _ = beta.Client.EC2.DeleteSnapshot(&ec2.DeleteSnapshotInput{
				SnapshotId: aws.String(betaSnap),
			})
			betaSnap = ""
		}

		time.Sleep(3 * time.Second)

		harness.Step(t, "delete volumes")
		if alphaVol != "" {
			_, _ = alpha.Client.EC2.DeleteVolume(&ec2.DeleteVolumeInput{
				VolumeId: aws.String(alphaVol),
			})
			alphaVol = ""
		}
		if betaVol != "" {
			_, _ = beta.Client.EC2.DeleteVolume(&ec2.DeleteVolumeInput{
				VolumeId: aws.String(betaVol),
			})
			betaVol = ""
		}

		harness.Step(t, "delete key pairs")
		for _, k := range alphaKeyNames {
			_, _ = alpha.Client.EC2.DeleteKeyPair(&ec2.DeleteKeyPairInput{KeyName: aws.String(k)})
		}
		alphaKeyNames = nil
		for _, k := range betaKeyNames {
			_, _ = beta.Client.EC2.DeleteKeyPair(&ec2.DeleteKeyPairInput{KeyName: aws.String(k)})
		}
		betaKeyNames = nil

		harness.Step(t, "delete EIGWs")
		if alphaEIGW != "" {
			_, _ = alpha.Client.EC2.DeleteEgressOnlyInternetGateway(&ec2.DeleteEgressOnlyInternetGatewayInput{
				EgressOnlyInternetGatewayId: aws.String(alphaEIGW),
			})
			alphaEIGW = ""
		}
		if betaEIGW != "" {
			_, _ = beta.Client.EC2.DeleteEgressOnlyInternetGateway(&ec2.DeleteEgressOnlyInternetGatewayInput{
				EgressOnlyInternetGatewayId: aws.String(betaEIGW),
			})
			betaEIGW = ""
		}

		harness.Step(t, "detach + delete IGWs")
		if alphaIGW != "" {
			if alphaVPC != "" {
				_, _ = alpha.Client.EC2.DetachInternetGateway(&ec2.DetachInternetGatewayInput{
					InternetGatewayId: aws.String(alphaIGW),
					VpcId:             aws.String(alphaVPC),
				})
			}
			_, _ = alpha.Client.EC2.DeleteInternetGateway(&ec2.DeleteInternetGatewayInput{
				InternetGatewayId: aws.String(alphaIGW),
			})
			alphaIGW = ""
		}
		if betaIGW != "" {
			_, _ = beta.Client.EC2.DeleteInternetGateway(&ec2.DeleteInternetGatewayInput{
				InternetGatewayId: aws.String(betaIGW),
			})
			betaIGW = ""
		}

		harness.Step(t, "delete subnets")
		if alphaSubnet != "" {
			_, _ = alpha.Client.EC2.DeleteSubnet(&ec2.DeleteSubnetInput{
				SubnetId: aws.String(alphaSubnet),
			})
			alphaSubnet = ""
		}
		if betaSubnet != "" {
			_, _ = beta.Client.EC2.DeleteSubnet(&ec2.DeleteSubnetInput{
				SubnetId: aws.String(betaSubnet),
			})
			betaSubnet = ""
		}

		harness.Step(t, "delete VPCs")
		if alphaVPC != "" {
			_, _ = alpha.Client.EC2.DeleteVpc(&ec2.DeleteVpcInput{VpcId: aws.String(alphaVPC)})
			alphaVPC = ""
		}
		if betaVPC != "" {
			_, _ = beta.Client.EC2.DeleteVpc(&ec2.DeleteVpcInput{VpcId: aws.String(betaVPC)})
			betaVPC = ""
		}

		// Clear the instance IDs so the outer t.Cleanup doesn't redundantly
		// terminate them (a second terminate on a terminated id is a no-op
		// at the gateway, but skipping the round-trip keeps logs clean).
		alphaInst = ""
		betaInst = ""
	})
}

// waitTerminated polls until id reaches "terminated" or vanishes (NotFound
// is treated as success — some gateway versions garbage-collect terminated
// rows). Bounded by a short timeout so a stuck termination surfaces fast.
func waitTerminated(t *testing.T, c *harness.AWSClient, id string) {
	t.Helper()
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		out, err := c.EC2.DescribeInstances(&ec2.DescribeInstancesInput{
			InstanceIds: []*string{aws.String(id)},
		})
		if err != nil {
			if aerr, ok := err.(awserr.Error); ok && aerr.Code() == "InvalidInstanceID.NotFound" {
				return
			}
			// Other errors — keep polling until the deadline.
		} else if len(out.Reservations) > 0 && len(out.Reservations[0].Instances) > 0 {
			state := aws.StringValue(out.Reservations[0].Instances[0].State.Name)
			if state == "terminated" {
				return
			}
		}
		time.Sleep(2 * time.Second)
	}
	// Don't fail the test from cleanup — log and move on.
	t.Logf("waitTerminated: %s did not reach terminated within 60s", id)
}

// describeInstanceIDs returns all instance ids visible to c across every
// reservation. Used for cross-account visibility assertions.
func describeInstanceIDs(t *testing.T, c *harness.AWSClient) []string {
	t.Helper()
	out, err := c.EC2.DescribeInstances(&ec2.DescribeInstancesInput{})
	require.NoError(t, err, "describe-instances")
	var ids []string
	for _, r := range out.Reservations {
		for _, i := range r.Instances {
			ids = append(ids, aws.StringValue(i.InstanceId))
		}
	}
	return ids
}

// describeVolumeIDs returns all volume ids visible to c.
func describeVolumeIDs(t *testing.T, c *harness.AWSClient) []string {
	t.Helper()
	out, err := c.EC2.DescribeVolumes(&ec2.DescribeVolumesInput{})
	require.NoError(t, err, "describe-volumes")
	ids := make([]string, 0, len(out.Volumes))
	for _, v := range out.Volumes {
		ids = append(ids, aws.StringValue(v.VolumeId))
	}
	return ids
}

// describeKeyPairNames returns key pair names visible to c.
func describeKeyPairNames(t *testing.T, c *harness.AWSClient) []string {
	t.Helper()
	out, err := c.EC2.DescribeKeyPairs(&ec2.DescribeKeyPairsInput{})
	require.NoError(t, err, "describe-key-pairs")
	names := make([]string, 0, len(out.KeyPairs))
	for _, kp := range out.KeyPairs {
		names = append(names, aws.StringValue(kp.KeyName))
	}
	return names
}

// describeKeyPairID looks up a specific key pair by name and returns its
// KeyPairId. Empty string if the key isn't found in c's namespace.
func describeKeyPairID(t *testing.T, c *harness.AWSClient, name string) string {
	t.Helper()
	out, err := c.EC2.DescribeKeyPairs(&ec2.DescribeKeyPairsInput{
		KeyNames: []*string{aws.String(name)},
	})
	if err != nil {
		// NotFound here is informational, not a test failure.
		return ""
	}
	if len(out.KeyPairs) == 0 {
		return ""
	}
	return aws.StringValue(out.KeyPairs[0].KeyPairId)
}

// vpcIDs flattens a slice of *ec2.Vpc to their ids.
func vpcIDs(vpcs []*ec2.Vpc) []string {
	ids := make([]string, 0, len(vpcs))
	for _, v := range vpcs {
		ids = append(ids, aws.StringValue(v.VpcId))
	}
	return ids
}

// describeRegionNames returns sorted region names. Bash compares by direct
// equality after a sort-by-position — same here, since the gateway returns
// the regions in a stable order.
func describeRegionNames(t *testing.T, c *harness.AWSClient) []string {
	t.Helper()
	out, err := c.EC2.DescribeRegions(&ec2.DescribeRegionsInput{})
	require.NoError(t, err, "describe-regions")
	names := make([]string, 0, len(out.Regions))
	for _, r := range out.Regions {
		names = append(names, aws.StringValue(r.RegionName))
	}
	return names
}

// describeAZNames returns AZ names.
func describeAZNames(t *testing.T, c *harness.AWSClient) []string {
	t.Helper()
	out, err := c.EC2.DescribeAvailabilityZones(&ec2.DescribeAvailabilityZonesInput{})
	require.NoError(t, err, "describe-availability-zones")
	names := make([]string, 0, len(out.AvailabilityZones))
	for _, z := range out.AvailabilityZones {
		names = append(names, aws.StringValue(z.ZoneName))
	}
	return names
}

// describeInstanceTypeNames returns sorted instance-type names — bash sorts
// before comparing so the assertion is order-insensitive.
func describeInstanceTypeNames(t *testing.T, c *harness.AWSClient) []string {
	t.Helper()
	out, err := c.EC2.DescribeInstanceTypes(&ec2.DescribeInstanceTypesInput{})
	require.NoError(t, err, "describe-instance-types")
	names := make([]string, 0, len(out.InstanceTypes))
	for _, it := range out.InstanceTypes {
		names = append(names, aws.StringValue(it.InstanceType))
	}
	// Sort to mirror bash's `| tr '\t' '\n' | sort` step.
	sortStrings(names)
	return names
}

// sortStrings sorts in place — kept local to avoid an extra import in the
// already-busy file. Equivalent to sort.Strings.
func sortStrings(xs []string) {
	for i := 1; i < len(xs); i++ {
		for j := i; j > 0 && xs[j-1] > xs[j]; j-- {
			xs[j-1], xs[j] = xs[j], xs[j-1]
		}
	}
}
