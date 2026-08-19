//go:build e2e

package quota

import (
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/aws/aws-sdk-go/service/elbv2"
	"github.com/aws/aws-sdk-go/service/rds"
	"github.com/mulgadc/spinifex/tests/e2e/harness"
	"github.com/stretchr/testify/require"
)

// A sandbox account is never configured, only defaulted, so the numbers it
// inherits are the ones production actually enforces. Every dimension must
// report as inherited: a value that silently arrived as an override would mean
// account creation is writing limits nobody asked it to write.
func TestQuotaBaselineIsInherited(t *testing.T) {
	fix := requireQuotaFixture(t)
	harness.Phase(t, "Quota — a new account inherits the configured baseline")

	limits, source := harness.SpxAdminQuotaGet(t, fix.Tenant.AccountID)

	want := map[string]int{
		"vcpus":          fix.Baseline.VCPUs,
		"vpcs":           fix.Baseline.VPCs,
		"subnets":        fix.Baseline.Subnets,
		"eips":           fix.Baseline.EIPs,
		"volumes":        fix.Baseline.Volumes,
		"volumes_gib":    fix.Baseline.VolumesGiB,
		"rds_instances":  fix.Baseline.RDSInstances,
		"load_balancers": fix.Baseline.LoadBalancers,
	}
	require.Len(t, limits, len(want), "every dimension must be reported")
	for dimension, value := range want {
		require.Equal(t, value, limits[dimension], "%s limit", dimension)
		require.Equal(t, "config", source[dimension], "%s must be inherited, not set", dimension)
	}
}

// The VPC gate, and with it the property the whole live-counted tier rests on:
// deleting a resource releases its allowance with no uncharge path to go wrong.
func TestQuotaVPCLimit(t *testing.T) {
	fix := requireQuotaFixture(t)
	harness.Phase(t, "Quota — VPCs")
	tenant := fix.Tenant.Client

	headroom(t, fix.Tenant.AccountID, "vpcs", countVPCs(t, tenant))

	harness.Step(t, "the one VPC the account has room for is created")
	vpcID := createVPC(t, tenant, "10.180.0.0/16")

	harness.Step(t, "the next is refused")
	// e2e:allow-create — the refusal is the assertion.
	_, err := tenant.EC2.CreateVpc(&ec2.CreateVpcInput{CidrBlock: aws.String("10.181.0.0/16")})
	harness.AssertAWSError(t, err, quotaExceeded)

	harness.Step(t, "deleting releases the allowance")
	deleteVPC(tenant, vpcID)
	released := createVPC(t, tenant, "10.182.0.0/16")
	require.NotEmpty(t, released, "a VPC deleted under the cap must free room for another")
}

// Subnets are capped per account rather than per VPC, so the count that matters
// spans every VPC the account holds.
func TestQuotaSubnetLimit(t *testing.T) {
	fix := requireQuotaFixture(t)
	harness.Phase(t, "Quota — subnets")
	tenant := fix.Tenant.Client

	vpcID := createVPC(t, tenant, "10.183.0.0/16")
	headroom(t, fix.Tenant.AccountID, "subnets", countSubnets(t, tenant))

	// e2e:allow-create — the create path is the subject under test.
	out, err := tenant.EC2.CreateSubnet(&ec2.CreateSubnetInput{
		VpcId:     aws.String(vpcID),
		CidrBlock: aws.String("10.183.1.0/24"),
	})
	require.NoError(t, err, "the subnet the account has room for")
	subnetID := aws.StringValue(out.Subnet.SubnetId)
	t.Cleanup(func() {
		_, _ = tenant.EC2.DeleteSubnet(&ec2.DeleteSubnetInput{SubnetId: aws.String(subnetID)})
	})

	// e2e:allow-create — the refusal is the assertion.
	_, err = tenant.EC2.CreateSubnet(&ec2.CreateSubnetInput{
		VpcId:     aws.String(vpcID),
		CidrBlock: aws.String("10.183.2.0/24"),
	})
	harness.AssertAWSError(t, err, quotaExceeded)
}

// Elastic IPs are the scarcest thing a cluster hands out — the pool is finite
// and shared — so this gate is the one that decides how many tenants fit.
//
// Both calls are queue-group requests answered by whichever node picks them up,
// so this leg also proves every node serves external IPAM: one that does not
// answers an empty address list the cap cannot count against.
func TestQuotaEIPLimit(t *testing.T) {
	fix := requireQuotaFixture(t)
	harness.Phase(t, "Quota — Elastic IPs")
	tenant := fix.Tenant.Client

	allocate := func() (*ec2.AllocateAddressOutput, error) {
		// e2e:allow-create — the allocation is the checked operation.
		return tenant.EC2.AllocateAddress(&ec2.AllocateAddressInput{Domain: aws.String("vpc")})
	}

	headroom(t, fix.Tenant.AccountID, "eips", countAddresses(t, tenant))

	alloc, err := allocate()
	requireEIPSupported(t, err)
	require.NoError(t, err, "the address the account has room for")
	allocID := aws.StringValue(alloc.AllocationId)
	released := false
	t.Cleanup(func() {
		if !released {
			_, _ = tenant.EC2.ReleaseAddress(&ec2.ReleaseAddressInput{AllocationId: aws.String(allocID)})
		}
	})

	_, err = allocate()
	requireEIPSupported(t, err)
	harness.AssertAWSError(t, err, quotaExceeded)

	harness.Step(t, "releasing the address frees the allowance")
	_, err = tenant.EC2.ReleaseAddress(&ec2.ReleaseAddressInput{AllocationId: aws.String(allocID)})
	require.NoError(t, err, "release-address")
	released = true

	regained, err := allocate()
	requireEIPSupported(t, err)
	require.NoError(t, err, "a released address must free room for another")
	t.Cleanup(func() {
		_, _ = tenant.EC2.ReleaseAddress(&ec2.ReleaseAddressInput{AllocationId: regained.AllocationId})
	})
}

// EBS is capped twice over, and the two caps catch different abuse: a tenant can
// exhaust per-volume overhead long before it exhausts capacity. Each must be
// able to refuse a create on its own.
func TestQuotaVolumeLimits(t *testing.T) {
	fix := requireQuotaFixture(t)
	harness.Phase(t, "Quota — EBS volumes")
	tenant := fix.Tenant.Client
	az := harness.DiscoverDefaultAZ(t, fix.Harness)

	t.Run("count", func(t *testing.T) {
		count, _ := volumeUsage(t, tenant)
		harness.SpxAdminQuotaSet(t, fix.Tenant.AccountID,
			"--volumes", itoa(count+1), "--volumes-gib", "-1")
		t.Cleanup(func() { harness.SpxAdminQuotaClear(t, fix.Tenant.AccountID) })

		volID := createVolume(t, tenant, az, 1)
		require.NotEmpty(t, volID)

		// e2e:allow-create — the refusal is the assertion.
		_, err := tenant.EC2.CreateVolume(&ec2.CreateVolumeInput{
			AvailabilityZone: aws.String(az), Size: aws.Int64(1),
		})
		harness.AssertAWSError(t, err, quotaExceeded)
	})

	t.Run("capacity", func(t *testing.T) {
		_, gib := volumeUsage(t, tenant)
		harness.SpxAdminQuotaSet(t, fix.Tenant.AccountID,
			"--volumes", "-1", "--volumes-gib", itoa(gib+1))
		t.Cleanup(func() { harness.SpxAdminQuotaClear(t, fix.Tenant.AccountID) })

		require.NotEmpty(t, createVolume(t, tenant, az, 1), "the GiB the account has room for")

		// e2e:allow-create — the refusal is the assertion.
		_, err := tenant.EC2.CreateVolume(&ec2.CreateVolumeInput{
			AvailabilityZone: aws.String(az), Size: aws.Int64(1),
		})
		harness.AssertAWSError(t, err, quotaExceeded)
	})
}

// The headline gate. A sandbox account is bounded by total vCPUs rather than by
// instance count, so it may spend its 16 however it likes — but not on one
// instance too wide to fit, not on a count of instances that sums past the cap,
// and not past the cap one at a time.
func TestQuotaVCPULimit(t *testing.T) {
	fix := requireQuotaFixture(t)
	harness.Phase(t, "Quota — EC2 vCPUs")
	tenant := fix.Tenant.Client

	nanoType, arch := harness.DiscoverNanoInstanceType(t, fix.Harness)
	nanoVCPUs := instanceTypeVCPUs(t, tenant, nanoType)
	spec := launchSpec{
		amiID:   harness.DiscoverUbuntuAMI(t, fix.Harness, arch),
		keyName: tenantKeyPair(t, tenant),
		subnet:  tenantSubnet(t, tenant),
	}

	t.Run("one instance wider than the cap is refused", func(t *testing.T) {
		wideType, wideVCPUs := widestInstanceType(t, tenant)
		require.Greater(t, wideVCPUs, fix.Baseline.VCPUs,
			"the widest offered type must exceed the cap or this asserts nothing")
		harness.Detail(t, "type", wideType, "vcpus", wideVCPUs, "cap", fix.Baseline.VCPUs)

		setLimit(t, fix.Tenant.AccountID, "vcpus", fix.Baseline.VCPUs)
		before := countInstances(t, tenant)

		_, err := spec.runInstances(tenant, wideType, 1)
		harness.AssertAWSError(t, err, quotaExceeded)

		require.Equal(t, before, countInstances(t, tenant),
			"a refused launch must not leave an instance behind")
	})

	// The count is multiplied by the type's vCPUs before the cap is consulted,
	// so a type that fits on its own must still be refused in bulk. Asserting
	// only the refusal would pass on a gateway that refuses everything, which is
	// why the same type is then launched at a count that does fit.
	t.Run("a launch is charged for every instance it asks for", func(t *testing.T) {
		const room = 2
		require.Zero(t, countInstances(t, tenant), "the tenant must start this subtest idle")
		harness.Detail(t, "type", nanoType, "vcpus_each", nanoVCPUs,
			"cap", room*nanoVCPUs, "refused_count", room+1)

		setLimit(t, fix.Tenant.AccountID, "vcpus", room*nanoVCPUs)

		_, err := spec.runInstances(tenant, nanoType, room+1)
		harness.AssertAWSError(t, err, quotaExceeded)

		out, err := spec.runInstances(tenant, nanoType, room)
		require.NoError(t, err, "a request that fits must be allowed")
		require.Len(t, out.Instances, room, "a permitted launch must deliver every instance")
		launched := registerTerminate(t, tenant, out)

		harness.Step(t, "the account is now at its cap")
		_, err = spec.runInstances(tenant, nanoType, 1)
		harness.AssertAWSError(t, err, quotaExceeded)

		harness.Step(t, "terminating releases the reserved vCPUs")
		for _, id := range launched {
			terminateInstance(tenant, id)
		}
		for _, id := range launched {
			waitTerminated(t, tenant, id)
		}

		// The counter is released by the gateway's periodic reconcile rather
		// than by the terminate itself, so the retry window spans a sweep. A
		// refused attempt launches nothing, so polling costs only the denials.
		harness.EventuallyErr(t, func() error {
			_, rerr := spec.launchOne(t, tenant, nanoType)
			return rerr
		}, 3*time.Minute, 10*time.Second)
	})
}

// Load balancers are refused at the gateway before any subnet is resolved, so
// the two halves of this test differ only in the limit: at zero the quota is
// what refuses, and uncapped the same call gets far enough to fail on its own
// bad input. That contrast is what proves the quota was the cause.
func TestQuotaLoadBalancerLimit(t *testing.T) {
	fix := requireQuotaFixture(t)
	harness.Phase(t, "Quota — load balancers")
	tenant := fix.Tenant.Client

	create := func() error {
		// The gate runs before the input is validated, which is what lets this
		// assert the limit without provisioning a balancer. e2e:allow-create
		_, err := tenant.ELBv2.CreateLoadBalancer(&elbv2.CreateLoadBalancerInput{
			Name:    aws.String("e2e-quota-lb"),
			Type:    aws.String("network"),
			Subnets: []*string{aws.String("subnet-00000000000000000")},
		})
		return err
	}

	setLimit(t, fix.Tenant.AccountID, "load-balancers", 0)
	harness.AssertAWSError(t, create(), quotaExceeded)

	harness.SpxAdminQuotaSet(t, fix.Tenant.AccountID, "--load-balancers", "-1")
	err := create()
	require.Error(t, err, "the bogus subnet must still be refused")
	require.False(t, harness.ErrorCodeIs(err, quotaExceeded),
		"an uncapped account must get past the quota gate, got: %v", err)
}

// RDS is capped by instance count rather than by vCPUs: the engine VM runs in
// the system account, so a tenant's database consumes none of the tenant's own
// vCPU allowance and the count is the only thing bounding it.
func TestQuotaRDSInstanceLimit(t *testing.T) {
	fix := requireQuotaFixture(t)
	harness.Phase(t, "Quota — RDS instances")
	tenant := fix.Tenant.Client

	create := func() error {
		// The gate runs before the input is validated, so the limit is asserted
		// without booting a database.
		_, err := tenant.RDS.CreateDBInstance(&rds.CreateDBInstanceInput{ //nolint:staticcheck // e2e:allow-create
			DBInstanceIdentifier: aws.String("e2e-quota-db"),
			DBInstanceClass:      aws.String("db.t3.micro"),
			Engine:               aws.String("mariadb"),
			MasterUsername:       aws.String("admin"),
			MasterUserPassword:   aws.String("e2e-quota-password"),
			AllocatedStorage:     aws.Int64(1),
			DBSubnetGroupName:    aws.String("e2e-quota-missing-subnet-group"),
		})
		return err
	}

	setLimit(t, fix.Tenant.AccountID, "rds-instances", 0)
	harness.AssertAWSError(t, create(), quotaExceeded)

	harness.SpxAdminQuotaSet(t, fix.Tenant.AccountID, "--rds-instances", "-1")
	err := create()
	require.Error(t, err, "the missing subnet group must still be refused")
	require.False(t, harness.ErrorCodeIs(err, quotaExceeded),
		"an uncapped account must get past the quota gate, got: %v", err)
}

// A limit is a property of one account, not of the gateway. Two tenants served
// by the same gateway, one capped and one not, is the whole reason overrides
// exist: a special customer is raised without loosening anybody else.
func TestQuotaOverrideIsPerAccount(t *testing.T) {
	fix := requireQuotaFixture(t)
	harness.Phase(t, "Quota — an override applies to one account only")

	setLimit(t, fix.Tenant.AccountID, "vpcs", 0)

	// e2e:allow-create — the refusal is the assertion.
	_, err := fix.Tenant.Client.EC2.CreateVpc(&ec2.CreateVpcInput{CidrBlock: aws.String("10.184.0.0/16")})
	harness.AssertAWSError(t, err, quotaExceeded)

	harness.Step(t, "the neighbouring account is untouched")
	require.NotEmpty(t, createVPC(t, fix.Peer.Client, "10.185.0.0/16"),
		"one account's override must not cap another")
}
