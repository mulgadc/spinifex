//go:build e2e

package rds

import (
	"fmt"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/rds"
	"github.com/mulgadc/spinifex/tests/e2e/harness"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	// The table the round-trip is written into. Shared by the subtests, because a
	// row that survives a reconnection is what proves they reached one database.
	connectTable = "e2e_connect"
	connectNote  = "hello from the client VM"

	// The rotation the master-password subtest moves to. Same character rules as
	// the original.
	rotatedPassword = "e2eR0tatedSecret2"

	// How long a security-group change has to take effect at the datapath. The
	// control-plane call returns as soon as the ENI is re-associated; the flow
	// tables behind it settle a moment later.
	sgSettleTimeout = 90 * time.Second
)

// TestConnectivity is the suite's client leg: a psql client inside the customer
// VPC, which is the only place a DB endpoint is reachable from, driven over SSH.
//
// Nothing here skips when the client is unavailable. A DB instance that reports
// available but accepts no connection is the failure this suite exists to catch,
// so a client VM that cannot be built fails the test rather than passing it
// quietly. The one sanctioned skip is a cluster with no base domain, where there
// is no endpoint name to resolve at all.
func TestConnectivity(t *testing.T) {
	f := requireRDSFixture(t)
	t.Parallel()
	reserveDBVMs(t, 1)

	id := fmt.Sprintf("%s-connect-%d", dbInstancePfx, time.Now().Unix())

	// Started before the client VM is built so the two boots overlap: the create
	// returns immediately and the engine bootstraps while apt runs in the client.
	harness.Phase(t, "Creating DB instance %q", id)
	createDBInstance(t, f, id)

	client := rdsClient(t, f)
	instance := waitForAvailable(t, f, id)

	endpoint := aws.StringValue(instance.Endpoint.Address)
	require.NotEmpty(t, endpoint, "an available instance must publish an endpoint")
	eni := harness.DBEndpointENI(t, f.AWS, id)
	privateIP := aws.StringValue(eni.PrivateIpAddress)
	require.NotEmpty(t, privateIP, "the endpoint ENI must carry a private address")

	byIP := harness.PSQLConn{
		Host: privateIP, Port: aws.Int64Value(instance.Endpoint.Port),
		User: dbMasterUser, Password: dbMasterPassword, DBName: dbName,
	}

	// The first assertion in the suite that proves an engine is actually serving:
	// every status before this one is the control plane's own account of itself.
	t.Run("AClientInTheVPCRoundTripsARow", func(t *testing.T) {
		harness.PSQL(t, client, byIP, fmt.Sprintf(
			"CREATE TABLE %s (id int primary key, note text); INSERT INTO %s VALUES (1, '%s');",
			connectTable, connectTable, connectNote))

		out := harness.PSQL(t, client, byIP, fmt.Sprintf("SELECT note FROM %s WHERE id = 1;", connectTable))
		assert.Equal(t, connectNote, strings.TrimSpace(out), "the row written over the endpoint must read back")
	})

	// D6: the name is the customer's handle on the instance, and it has to resolve
	// where a customer resolves it — inside the VPC. A lookup on the runner proves
	// nothing, because the runner's resolver may point straight at northstar.
	t.Run("TheEndpointNameResolvesInTheGuest", func(t *testing.T) {
		requireEndpointName(t, endpoint)
		assert.Equal(t, fmt.Sprintf("%s.%s.%s.rds.%s", id, f.Account, f.Region, f.BaseDomain), endpoint,
			"the endpoint name is account-qualified so identifiers collide across tenants without colliding in DNS")

		addrs := harness.ResolveInGuest(t, client, endpoint)
		assert.Contains(t, addrs, privateIP, "the endpoint name must resolve to the customer ENI's address")

		byName := byIP
		byName.Host = endpoint
		out := harness.PSQL(t, client, byName, fmt.Sprintf("SELECT note FROM %s WHERE id = 1;", connectTable))
		assert.Equal(t, connectNote, strings.TrimSpace(out),
			"the name and the address must reach the same database")
	})

	// D14 puts both the vanity name and the ENI address in the serving
	// certificate's SAN set specifically so verify-full works either way, and that
	// is the mode a customer who cares about TLS actually uses.
	t.Run("VerifyFullSucceedsByNameAndByIP", func(t *testing.T) {
		hosts := map[string]string{"ByIP": privateIP}
		if net.ParseIP(endpoint) == nil {
			hosts["ByName"] = endpoint
		}
		for name, host := range hosts {
			t.Run(name, func(t *testing.T) {
				conn := byIP
				conn.Host = host
				conn.SSLMode = "verify-full"
				conn.SSLRootCert = harness.RDSClientCACertPath

				out := harness.PSQL(t, client, conn,
					"SELECT ssl FROM pg_stat_ssl WHERE pid = pg_backend_pid();")
				assert.Equal(t, "t", strings.TrimSpace(out),
					"a verify-full connection must report itself encrypted")
			})
		}
	})

	// The endpoint ENI is injected into the customer's subnet, so the only thing
	// that can gate it is a security group. Nothing else in the suite proves the
	// groups a customer sets are in force on it.
	t.Run("ASecurityGroupGatesTheEndpoint", func(t *testing.T) {
		original := securityGroupIDs(instance)
		require.NotEmpty(t, original, "the instance must report the groups its ENI carries")

		vpcID := aws.StringValue(instance.DBSubnetGroup.VpcId)
		closed := harness.EnsureSG(t, f.Harness, vpcID, "rds-e2e-closed")

		harness.Step(t, "Re-associating %q onto a group that does not admit 5432", id)
		setSecurityGroups(t, f, id, []string{closed})
		// Restored inside the subtest as well as asserted below: a failure part-way
		// would otherwise leave every later test connecting through a closed group.
		t.Cleanup(func() { setSecurityGroups(t, f, id, original) })

		harness.EventuallyErr(t, func() error {
			if _, err := harness.TryPSQL(client, byIP, "SELECT 1;"); err != nil {
				return nil
			}
			return fmt.Errorf("%s still accepts connections through a group that does not admit %d", id, harness.DBEnginePort)
		}, sgSettleTimeout, 5*time.Second)

		harness.Step(t, "Restoring the original groups on %q", id)
		setSecurityGroups(t, f, id, original)
		harness.EventuallyErr(t, func() error {
			_, err := harness.TryPSQL(client, byIP, "SELECT 1;")
			return err
		}, sgSettleTimeout, 5*time.Second)
	})

	// Last: it retires the credential every subtest above connects with. D8 keeps
	// no cleartext password anywhere, so the only proof the rotation reached the
	// engine is that the new one authenticates and the old one does not.
	t.Run("TheMasterPasswordRotates", func(t *testing.T) {
		_, err := f.AWS.RDS.ModifyDBInstance(&rds.ModifyDBInstanceInput{
			DBInstanceIdentifier: aws.String(id),
			MasterUserPassword:   aws.String(rotatedPassword),
			ApplyImmediately:     aws.Bool(true),
		})
		require.NoError(t, err, "modify-db-instance master password")

		rotated := byIP
		rotated.Password = rotatedPassword
		out := harness.PSQL(t, client, rotated, fmt.Sprintf("SELECT note FROM %s WHERE id = 1;", connectTable))
		assert.Equal(t, connectNote, strings.TrimSpace(out), "the rotated password must reach the same database")

		out, err = harness.TryPSQL(client, byIP, "SELECT 1;")
		require.Error(t, err, "the retired password must not connect: %s", out)
		assert.Contains(t, out, "authentication",
			"the old password must be refused by the engine, not by the network")
	})
}

// The suite's one sanctioned skip on the client leg: with no base domain the
// endpoint is the bare ENI address, so there is no name to resolve and nothing
// a hosts-file entry could honestly stand in for.
func requireEndpointName(t *testing.T, endpoint string) {
	t.Helper()
	if net.ParseIP(endpoint) != nil {
		t.Skipf("endpoint %s is a bare ENI address; no northstar base domain is configured", endpoint)
	}
}

func securityGroupIDs(instance *rds.DBInstance) []string {
	ids := make([]string, 0, len(instance.VpcSecurityGroups))
	for _, g := range instance.VpcSecurityGroups {
		ids = append(ids, aws.StringValue(g.VpcSecurityGroupId))
	}
	return ids
}

// Re-associates the endpoint ENI's groups. Non-disruptive, so it lands
// immediately whatever ApplyImmediately says, and the call returns before the
// datapath has caught up — hence the polls at every call site.
func setSecurityGroups(t *testing.T, f *Fixture, id string, groups []string) {
	t.Helper()
	out, err := f.AWS.RDS.ModifyDBInstance(&rds.ModifyDBInstanceInput{
		DBInstanceIdentifier: aws.String(id),
		VpcSecurityGroupIds:  aws.StringSlice(groups),
		ApplyImmediately:     aws.Bool(true),
	})
	if err != nil {
		t.Fatalf("modify-db-instance security groups %v: %v", groups, err)
	}
	assert.ElementsMatch(t, groups, securityGroupIDs(out.DBInstance),
		"the describe must report the groups the ENI now carries")
}
