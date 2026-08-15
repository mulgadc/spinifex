//go:build e2e

package rds

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/aws/aws-sdk-go/service/rds"
	"github.com/mulgadc/spinifex/tests/e2e/harness"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	// The topology's own address space. A dedicated VPC rather than a second
	// subnet in the default one: EnsureDefaultVPC and DiscoverDefaultVPC both
	// take the first subnet DescribeSubnets happens to return, which is stable
	// only while there is one, and TestSubnetAndParameterGroups builds a subnet
	// group over every subnet the default VPC has.
	crossSubnetVPCCIDR    = "10.42.0.0/16"
	crossSubnetClientCIDR = "10.42.1.0/24"
	crossSubnetDBCIDR     = "10.42.2.0/24"

	crossSubnetTable = "e2e_cross_subnet"
	crossSubnetNote  = "hello from the next subnet over"
)

// crossSubnetNet is the topology the test owns: a client subnet with a public
// path for SSH and apt, and a DB subnet with no route table of its own, which
// leaves it on the main table CreateVpc writes — intra-VPC routing and nothing
// else, exactly what a private DB subnet is.
type crossSubnetNet struct {
	VPCID          string
	ClientSubnetID string
	DBSubnetID     string
	ClientSGID     string
	DBSGID         string
}

// TestCrossSubnetConnectivity is the suite's second client leg, and the only one
// that proves the endpoint is reachable from the VPC rather than from the
// endpoint ENI's own subnet.
//
// The distinction is invisible to every other test here: a client on the ENI's
// subnet is answered on-link, with no routing decision to get wrong, and the
// whole suite has only ever had one subnet in play. A client anywhere else needs
// the DB VM to route the reply back out the ENI it arrived on, which is the
// asymmetry a dual-NIC guest gets wrong by default.
func TestCrossSubnetConnectivity(t *testing.T) {
	f := requireRDSFixture(t)
	t.Parallel()
	reserveDBVMs(t, dbClass)

	suffix := time.Now().Unix()
	id := fmt.Sprintf("%s-xsubnet-%d", dbInstancePfx, suffix)
	subnetGroup := fmt.Sprintf("%s-xsubnet-%d", dbInstancePfx, suffix)

	topo := setupCrossSubnetVPC(t, f)

	harness.Phase(t, "Creating DB subnet group %q over the DB subnet alone", subnetGroup)
	createDBSubnetGroup(t, f, subnetGroup, topo.DBSubnetID)

	// Started before the client VM so the two boots overlap, as TestConnectivity
	// does: the create returns immediately and the engine bootstraps while apt
	// runs in the client.
	harness.Phase(t, "Creating DB instance %q in the DB subnet", id)
	createDBInstance(t, f, id, func(in *rds.CreateDBInstanceInput) {
		in.DBSubnetGroupName = aws.String(subnetGroup)
		in.VpcSecurityGroupIds = aws.StringSlice([]string{topo.DBSGID})
	})

	// A fixture bound to this test, not the process one the shared default-VPC
	// client hangs off: this VM has to be terminated before the cleanup below it
	// deletes the subnet it is sitting in, and the process fixture drains after
	// every test in the package has finished.
	clientFx := harness.NewFixture(t, f.AWS)
	client := harness.RDSClientVMIn(t, f.AWS, clientFx, f.Env,
		harness.RDSClientPlacement{SubnetID: topo.ClientSubnetID, SGID: topo.ClientSGID})

	instance := waitForAvailable(t, f, id)
	endpoint := aws.StringValue(instance.Endpoint.Address)
	require.NotEmpty(t, endpoint, "an available instance must publish an endpoint")

	eni := harness.DBEndpointENI(t, f.AWS, id)
	privateIP := aws.StringValue(eni.PrivateIpAddress)
	require.NotEmpty(t, privateIP, "the endpoint ENI must carry a private address")

	// Without this the test could pass for the wrong reason: a subnet group over
	// one subnet is the only thing pinning the endpoint away from the client, and
	// an endpoint that landed in the client's subnet would be the same
	// same-subnet case TestConnectivity already covers.
	require.Equal(t, topo.DBSubnetID, aws.StringValue(eni.SubnetId),
		"the subnet group has one subnet, so the endpoint ENI must be in it")

	conn := harness.PSQLConn{
		Host: privateIP, Port: aws.Int64Value(instance.Endpoint.Port),
		User: dbMasterUser, Password: dbMasterPassword, DBName: dbName,
	}

	// The assertion the whole test exists for. It fails as a connect timeout,
	// which is indistinguishable from a security-group denial at the client — so
	// the group here admits the client subnet outright, leaving the return path
	// as the only thing left to be wrong.
	t.Run("AClientInAnotherSubnetRoundTripsARow", func(t *testing.T) {
		harness.PSQL(t, client, conn, fmt.Sprintf(
			"CREATE TABLE %s (id int primary key, note text); INSERT INTO %s VALUES (1, '%s');",
			crossSubnetTable, crossSubnetTable, crossSubnetNote))

		out := harness.PSQL(t, client, conn, fmt.Sprintf("SELECT note FROM %s WHERE id = 1;", crossSubnetTable))
		assert.Equal(t, crossSubnetNote, strings.TrimSpace(out),
			"a row written from outside the endpoint's own subnet must read back")
	})

	// The address proves the datapath; the name is what an application tier in
	// its own subnet is actually configured with.
	t.Run("TheEndpointNameReachesTheSameDatabase", func(t *testing.T) {
		requireEndpointName(t, endpoint)

		addrs := harness.ResolveInGuest(t, client, endpoint)
		assert.Contains(t, addrs, privateIP, "the endpoint name must resolve to the customer ENI's address")

		byName := conn
		byName.Host = endpoint
		out := harness.PSQL(t, client, byName, fmt.Sprintf("SELECT note FROM %s WHERE id = 1;", crossSubnetTable))
		assert.Equal(t, crossSubnetNote, strings.TrimSpace(out),
			"the name and the address must reach the same database across subnets")
	})
}

// setupCrossSubnetVPC builds the two-subnet VPC and registers its teardown. No
// CreateWorkerEgress: only the client needs egress and it has a public IP, so
// the NAT gateway that helper also builds would be waited on for nothing.
func setupCrossSubnetVPC(t *testing.T, f *Fixture) crossSubnetNet {
	t.Helper()
	c := f.AWS

	harness.Phase(t, "Creating a VPC with a client subnet and a private DB subnet")
	topo := crossSubnetNet{VPCID: harness.CreateVPC(t, c, crossSubnetVPCCIDR)}
	t.Cleanup(func() { harness.DeleteVPC(t, c, topo.VPCID) })

	topo.ClientSubnetID = harness.CreateSubnet(t, c, topo.VPCID, crossSubnetClientCIDR)
	t.Cleanup(func() { harness.DeleteSubnet(t, c, topo.ClientSubnetID) })
	topo.DBSubnetID = harness.CreateSubnet(t, c, topo.VPCID, crossSubnetDBCIDR)
	t.Cleanup(func() { harness.DeleteSubnet(t, c, topo.DBSubnetID) })

	// The client needs a public IP for the runner's SSH and for apt. The DB
	// subnet gets neither that nor a route table association: it keeps the main
	// table, which routes inside the VPC and nowhere else.
	_, err := c.EC2.ModifySubnetAttribute(&ec2.ModifySubnetAttributeInput{
		SubnetId:            aws.String(topo.ClientSubnetID),
		MapPublicIpOnLaunch: &ec2.AttributeBooleanValue{Value: aws.Bool(true)},
	})
	require.NoError(t, err, "modify-subnet-attribute MapPublicIpOnLaunch")

	igw, err := c.EC2.CreateInternetGateway(&ec2.CreateInternetGatewayInput{}) // e2e:allow-create — the client subnet's own path out.
	require.NoError(t, err, "create-internet-gateway")
	igwID := aws.StringValue(igw.InternetGateway.InternetGatewayId)
	t.Cleanup(func() {
		if _, derr := c.EC2.DetachInternetGateway(&ec2.DetachInternetGatewayInput{
			InternetGatewayId: aws.String(igwID), VpcId: aws.String(topo.VPCID),
		}); derr != nil {
			t.Logf("detach igw %s: %v", igwID, derr)
		}
		if _, derr := c.EC2.DeleteInternetGateway(&ec2.DeleteInternetGatewayInput{
			InternetGatewayId: aws.String(igwID),
		}); derr != nil {
			t.Logf("delete igw %s: %v", igwID, derr)
		}
	})
	_, err = c.EC2.AttachInternetGateway(&ec2.AttachInternetGatewayInput{
		InternetGatewayId: aws.String(igwID), VpcId: aws.String(topo.VPCID),
	})
	require.NoError(t, err, "attach-internet-gateway")

	// The default route goes on a table associated with the client subnet, not on
	// the main table: putting it on main would give the DB subnet a way out too,
	// and a private DB subnet is the layout under test.
	rt, err := c.EC2.CreateRouteTable(&ec2.CreateRouteTableInput{VpcId: aws.String(topo.VPCID)}) // e2e:allow-create — the client subnet's public table.
	require.NoError(t, err, "create-route-table")
	rtID := aws.StringValue(rt.RouteTable.RouteTableId)
	t.Cleanup(func() {
		if _, derr := c.EC2.DeleteRouteTable(&ec2.DeleteRouteTableInput{RouteTableId: aws.String(rtID)}); derr != nil {
			t.Logf("delete route table %s: %v", rtID, derr)
		}
	})
	_, err = c.EC2.CreateRoute(&ec2.CreateRouteInput{
		RouteTableId:         aws.String(rtID),
		DestinationCidrBlock: aws.String("0.0.0.0/0"),
		GatewayId:            aws.String(igwID),
	})
	require.NoError(t, err, "create-route 0.0.0.0/0 -> igw")

	assoc, err := c.EC2.AssociateRouteTable(&ec2.AssociateRouteTableInput{
		RouteTableId: aws.String(rtID), SubnetId: aws.String(topo.ClientSubnetID),
	})
	require.NoError(t, err, "associate-route-table")
	assocID := aws.StringValue(assoc.AssociationId)
	t.Cleanup(func() {
		if _, derr := c.EC2.DisassociateRouteTable(&ec2.DisassociateRouteTableInput{
			AssociationId: aws.String(assocID),
		}); derr != nil {
			t.Logf("disassociate route table %s: %v", assocID, derr)
		}
	})

	topo.ClientSGID = createCrossSubnetSG(t, f, topo.VPCID, "client", crossSubnetSSHIngress())
	topo.DBSGID = createCrossSubnetSG(t, f, topo.VPCID, "db", crossSubnetDBIngress())

	t.Logf("cross-subnet topology: vpc=%s client=%s (%s) db=%s (%s)",
		topo.VPCID, topo.ClientSubnetID, crossSubnetClientCIDR, topo.DBSubnetID, crossSubnetDBCIDR)
	return topo
}

// The runner reaches the client over SSH from wherever CI happens to run it.
func crossSubnetSSHIngress() *ec2.IpPermission {
	return &ec2.IpPermission{
		IpProtocol: aws.String("tcp"),
		FromPort:   aws.Int64(22),
		ToPort:     aws.Int64(22),
		IpRanges:   []*ec2.IpRange{{CidrIp: aws.String("0.0.0.0/0")}},
	}
}

// The endpoint admits the client subnet by CIDR. A source group would be the
// same rule via a membership the control plane derives, and this test is about
// the return path: a denial and a missing route both present as a timeout, so
// the group is the one part left with nothing to get wrong.
func crossSubnetDBIngress() *ec2.IpPermission {
	return &ec2.IpPermission{
		IpProtocol: aws.String("tcp"),
		FromPort:   aws.Int64(harness.PostgresEnginePort),
		ToPort:     aws.Int64(harness.PostgresEnginePort),
		IpRanges:   []*ec2.IpRange{{CidrIp: aws.String(crossSubnetClientCIDR)}},
	}
}

func createCrossSubnetSG(t *testing.T, f *Fixture, vpcID, role string, ingress *ec2.IpPermission) string {
	t.Helper()
	name := fmt.Sprintf("%s-xsubnet-%s-%d", dbInstancePfx, role, time.Now().UnixNano())
	out, err := f.AWS.EC2.CreateSecurityGroup(&ec2.CreateSecurityGroupInput{ // e2e:allow-create — this test's own VPC, so no fixture group fits.
		VpcId:       aws.String(vpcID),
		GroupName:   aws.String(name),
		Description: aws.String("rds e2e cross-subnet " + role),
	})
	require.NoError(t, err, "create-security-group %s", name)
	sgID := aws.StringValue(out.GroupId)
	t.Cleanup(func() {
		if _, derr := f.AWS.EC2.DeleteSecurityGroup(&ec2.DeleteSecurityGroupInput{
			GroupId: aws.String(sgID),
		}); derr != nil {
			t.Logf("delete sg %s: %v", sgID, derr)
		}
	})

	_, err = f.AWS.EC2.AuthorizeSecurityGroupIngress(&ec2.AuthorizeSecurityGroupIngressInput{
		GroupId:       aws.String(sgID),
		IpPermissions: []*ec2.IpPermission{ingress},
	})
	require.NoError(t, err, "authorize-security-group-ingress on %s", sgID)
	return sgID
}

// A DB subnet group over exactly the subnets named, which is what places the
// endpoint ENI somewhere the client is not.
func createDBSubnetGroup(t *testing.T, f *Fixture, name string, subnetIDs ...string) {
	t.Helper()
	out, err := f.AWS.RDS.CreateDBSubnetGroup(&rds.CreateDBSubnetGroupInput{
		DBSubnetGroupName:        aws.String(name),
		DBSubnetGroupDescription: aws.String("rds e2e cross-subnet group"),
		SubnetIds:                aws.StringSlice(subnetIDs),
	})
	require.NoError(t, err, "create-db-subnet-group %s", name)
	require.NotNil(t, out.DBSubnetGroup)
	t.Cleanup(func() {
		if _, derr := f.AWS.RDS.DeleteDBSubnetGroup(&rds.DeleteDBSubnetGroupInput{
			DBSubnetGroupName: aws.String(name),
		}); derr != nil && !harness.ErrorCodeIs(derr, "DBSubnetGroupNotFoundFault") {
			t.Logf("cleanup: delete-db-subnet-group %s: %v", name, derr)
		}
	})
}
