//go:build e2e

package rds

import (
	"errors"
	"fmt"
	"net"
	"syscall"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	handlers_rds "github.com/mulgadc/spinifex/spinifex/handlers/rds"
	"github.com/mulgadc/spinifex/tests/e2e/harness"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Long enough that a slow datapath is not read as a refusal, short enough that
// four probes do not dominate the test.
const isolationDialTimeout = 5 * time.Second

// TestDBVMIsolation asserts that a DB instance's endpoint is the only path to
// its engine.
//
// A DB VM has three NICs. Only one of them — the customer ENI the endpoint
// resolves to — is governed by a customer security group. The other two are the
// system NIC in the RDS system VPC every DB VM in the deployment shares, and a
// management NIC on the host's br-mgmt bridge, which is plain L2 with no ACLs at
// all. An engine bound to the wildcard is therefore reachable on both, from any
// account, and the customer's security group is not consulted.
//
// Two DB instances in two tenants, because one instance can only show that a
// path is closed to the runner; two show it is closed between tenants, which is
// what the exposure actually was.
func TestDBVMIsolation(t *testing.T) {
	f := requireRDSFixture(t)
	t.Parallel()
	reserveDBVMs(t, dbClass, dbClass)

	suffix := time.Now().Unix()
	mine := fmt.Sprintf("%s-iso-a-%d", dbInstancePfx, suffix)
	theirs := fmt.Sprintf("%s-iso-b-%d", dbInstancePfx, suffix)

	harness.Phase(t, "Creating DB instance %q in this tenant", mine)
	createDBInstance(t, f, mine)

	harness.Phase(t, "Creating DB instance %q in a second tenant", theirs)
	other := isolationSecondTenant(t, f, suffix)
	_, err := other.RDS.CreateDBInstance(validCreateInput(theirs)) //nolint:staticcheck // e2e:allow-create — the second tenant's own instance
	require.NoError(t, err, "create-db-instance %s in the second tenant", theirs)
	t.Cleanup(func() { deleteInstanceAs(t, other, theirs) })

	instance := waitForAvailable(t, f, mine)
	harness.WaitForDBInstanceAvailable(t, other, theirs)
	enginePort := aws.Int64Value(instance.Endpoint.Port)
	require.NotZero(t, enginePort, "an available instance must publish an endpoint port")

	system := f.SystemAWS(t)

	// The direct proof, and the only leg that covers the co-location path: the
	// runner sits on br-mgmt and can address any DB VM's management NIC on it,
	// with no security group and no VPC in the way. A wildcard bind answers here.
	t.Run("TheEngineIsNotBoundOnTheManagementBridge", func(t *testing.T) {
		for _, id := range []string{mine, theirs} {
			mgmtIP := harness.DBInstanceMgmtIP(t, f.Env, system, id)
			requireRunnerOnMgmtBridge(t, mgmtIP)

			conn, err := net.DialTimeout("tcp", net.JoinHostPort(mgmtIP, fmt.Sprint(enginePort)), isolationDialTimeout)
			if err == nil {
				_ = conn.Close()
				t.Errorf("%s: the engine answered on the management bridge at %s:%d; "+
					"that NIC is governed by no security group, so this is reachable from every DB VM on the host",
					id, mgmtIP, enginePort)
				continue
			}
			if !errors.Is(err, syscall.ECONNREFUSED) {
				t.Errorf("%s: management bridge %s:%d could not be tested: want connection refused, got %v",
					id, mgmtIP, enginePort, err)
				continue
			}
			t.Logf("%s: management bridge %s:%d refused as expected", id, mgmtIP, enginePort)
		}
	})

	// The second layer, asserted from the control plane: even a wildcard bind must
	// not be admitted on the system NIC. Both instances' system NICs live in the
	// same shared VPC, so the group they carry is what separates them.
	t.Run("TheSystemNICCarriesAnIngressFreeGroup", func(t *testing.T) {
		want := handlers_rds.SystemSecurityGroupName(f.Region)

		var vpcIDs []string
		for _, id := range []string{mine, theirs} {
			eni := harness.DBSystemENI(t, system, id)
			vpcIDs = append(vpcIDs, aws.StringValue(eni.VpcId))

			require.Len(t, eni.Groups, 1, "%s: the system NIC must carry exactly one group", id)
			group := isolationDescribeSG(t, system, aws.StringValue(eni.Groups[0].GroupId))

			// Landing in the VPC's default group is the whole of the exposure: its
			// one ingress rule admits every other member of itself, which in this
			// VPC is every DB VM in the deployment, across every account.
			assert.Equal(t, want, aws.StringValue(group.GroupName),
				"%s: the system NIC must carry the RDS system group, not the VPC default", id)
			assert.Empty(t, group.IpPermissions,
				"%s: nothing dials a DB VM's system NIC, so its group must authorize no ingress", id)
		}

		require.Len(t, vpcIDs, 2)
		assert.Equal(t, vpcIDs[0], vpcIDs[1],
			"both tenants' system NICs share one VPC, which is why the group has to be the boundary")
	})

	// The regression half: closing those paths must not have closed the one the
	// customer actually uses.
	t.Run("TheEndpointStillReachesTheEngine", func(t *testing.T) {
		client := rdsClient(t, f)
		out := harness.PSQL(t, client, harness.PSQLConnFor(t, instance, dbMasterUser, dbMasterPassword, dbName),
			"SELECT 1;")
		assert.Contains(t, out, "1", "the endpoint must still serve the customer it is bound for")
	})
}

// A second tenant with its own account, so the two DB VMs share nothing but the
// platform underneath them.
func isolationSecondTenant(t *testing.T, f *Fixture, suffix int64) *harness.AWSClient {
	t.Helper()
	carousel := harness.NewAccountCarousel()
	tenant := carousel.Add(t, f.Env, "rds-e2e-isolation",
		harness.SpxAdminAccountCreate(t, fmt.Sprintf("RDS E2E Isolation %d", suffix), ""))
	return tenant.Client
}

// A refused connection proves nothing if the runner has no route to br-mgmt at
// all, so the probe's premise is checked rather than assumed: the bridge is one
// flat segment across the cluster, and the runner holds an address on it.
func requireRunnerOnMgmtBridge(t *testing.T, mgmtIP string) {
	t.Helper()
	target := net.ParseIP(mgmtIP)
	require.NotNil(t, target, "management address %q is not an IP", mgmtIP)

	addrs, err := net.InterfaceAddrs()
	require.NoError(t, err, "read the runner's own interface addresses")
	for _, addr := range addrs {
		if ipnet, ok := addr.(*net.IPNet); ok && ipnet.Contains(target) {
			return
		}
	}
	t.Fatalf("the runner holds no address on the segment carrying %s, so a refused connection "+
		"would prove a missing route rather than an unbound engine", mgmtIP)
}

func isolationDescribeSG(t *testing.T, system *harness.AWSClient, groupID string) *ec2.SecurityGroup {
	t.Helper()
	out, err := system.EC2.DescribeSecurityGroups(&ec2.DescribeSecurityGroupsInput{
		GroupIds: []*string{aws.String(groupID)},
	})
	require.NoError(t, err, "describe-security-groups %s", groupID)
	require.Len(t, out.SecurityGroups, 1, "describe-security-groups %s returned no group", groupID)
	return out.SecurityGroups[0]
}
