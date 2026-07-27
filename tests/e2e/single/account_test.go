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

// runAccountScoping validates that EC2 resources (instances, volumes, key
// pairs, snapshots, VPCs, IGWs, EIGWs) are isolated between tenant accounts.
// Three accounts are created (Alpha, Beta, Gamma). All resources use scoped
// clients with static credentials. t.Cleanup tears down everything best-effort.
func runAccountScoping(t *testing.T, fix *Fixture) {
	harness.Phase(t, "Single — EC2 Account Scoping")

	amiID := needAMI(t, fix)
	instType, _ := needInstanceTypeArch(t, fix)
	az := needAZ(t, fix)

	carousel := harness.NewAccountCarousel()

	// State threaded across Steps; zero values are skipped by teardown.
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

	// t.Cleanup runs LIFO. All deletes below swallow not-found errors (idempotent).
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

	// Every subsequent step derefs these; bail if Step 1 failed to register them.
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
	// Step 4 (key-pair scoping) is covered by tests/integration's
	// TestAccountScoping_KeyPairs — KeyServiceImpl's ownership check is a
	// plain account-prefixed S3 key ("keys/<accountID>/..."), no vm.Manager or
	// real guest involved, so the live import-key-pair variant proved nothing
	// beyond what the integration tier already asserts (CreateKeyPair/
	// DeleteKeyPair/namespace-isolation across two real, independently minted
	// accounts). ImportKeyPair isolation is not separately re-asserted there:
	// it shares the same account-prefixed storage path as CreateKeyPair, so a
	// second create-time isolation check would exercise identical code.

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

		// DeleteSnapshot returns UnauthorizedOperation (not NotFound) across accounts.
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

	// Steps 6/7/8 (VPC/subnet, IGW/EIGW, and account-settings scoping) are
	// covered by tests/integration's TestAccountScoping_VPCSubnet,
	// TestAccountScoping_IGWEIGW and TestAccountScoping_Settings.
	// VPCServiceImpl/IGWServiceImpl/EgressOnlyIGWServiceImpl/
	// AccountSettingsServiceImpl all resolve ownership from a plain
	// account-scoped KV key (utils.AccountKey(accountID, id) or an
	// account-keyed settings record) — no vm.Manager, no real guest, no OVN
	// state that only exists once a VPC has an attached instance. The live
	// variants proved nothing beyond what the integration tier now asserts
	// across two real, independently minted tenant accounts.

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
	// Explicit teardown — outer t.Cleanup also runs idempotently on failure.
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

		alphaInst = ""
		betaInst = ""
	})
}

// waitTerminated polls until id reaches "terminated" or NotFound (60s cap).
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

// describeRegionNames returns region names in gateway-returned order.
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

// describeInstanceTypeNames returns sorted instance-type names.
func describeInstanceTypeNames(t *testing.T, c *harness.AWSClient) []string {
	t.Helper()
	out, err := c.EC2.DescribeInstanceTypes(&ec2.DescribeInstanceTypesInput{})
	require.NoError(t, err, "describe-instance-types")
	names := make([]string, 0, len(out.InstanceTypes))
	for _, it := range out.InstanceTypes {
		names = append(names, aws.StringValue(it.InstanceType))
	}
	sortStrings(names)
	return names
}

// sortStrings sorts xs in place (insertion sort).
func sortStrings(xs []string) {
	for i := 1; i < len(xs); i++ {
		for j := i; j > 0 && xs[j-1] > xs[j]; j-- {
			xs[j-1], xs[j] = xs[j], xs[j-1]
		}
	}
}
