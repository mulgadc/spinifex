package handlers_rds

import (
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/rds"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	handlers_dns "github.com/mulgadc/spinifex/spinifex/handlers/dns"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// seedCreated runs a create for id and returns the stored record, so the
// describe is asserted against what a real create actually leaves behind.
func seedCreated(t *testing.T, h *createHarness, id string) DBInstanceRecord {
	t.Helper()
	input := validCreateInput()
	input.DBInstanceIdentifier = aws.String(id)
	_, err := h.svc.CreateDBInstance(t.Context(), input, testAccountID)
	require.NoError(t, err)
	return h.record(t, id)
}

func TestDescribeDBInstances_ListsEveryInstanceSorted(t *testing.T) {
	h := newCreateHarness(t, "")
	for _, id := range []string{"zulu-db", "alpha-db", "mike-db"} {
		seedCreated(t, h, id)
	}

	out, err := h.svc.DescribeDBInstances(t.Context(), &rds.DescribeDBInstancesInput{}, testAccountID)
	require.NoError(t, err)

	got := make([]string, 0, len(out.DBInstances))
	for _, instance := range out.DBInstances {
		got = append(got, aws.StringValue(instance.DBInstanceIdentifier))
	}
	assert.Equal(t, []string{"alpha-db", "mike-db", "zulu-db"}, got,
		"an unfiltered describe is ordered so paging a fleet is stable")
}

func TestDescribeDBInstances_EmptyAccountIsAnEmptyList(t *testing.T) {
	h := newCreateHarness(t, "")

	out, err := h.svc.DescribeDBInstances(t.Context(), nil, testAccountID)
	require.NoError(t, err)
	assert.Empty(t, out.DBInstances)
}

// A client polling a create must be able to tell "not ready" from "gone", so a
// named instance that does not exist is an error rather than an empty list.
func TestDescribeDBInstances_NamedMissingInstanceIsAnError(t *testing.T) {
	h := newCreateHarness(t, "")
	seedCreated(t, h, testDBInstanceID)

	_, err := h.svc.DescribeDBInstances(t.Context(), &rds.DescribeDBInstancesInput{
		DBInstanceIdentifier: aws.String("no-such-db"),
	}, testAccountID)
	require.Error(t, err)
	assert.Contains(t, err.Error(), awserrors.ErrorDBInstanceNotFound)
}

func TestDescribeDBInstances_NamedInstanceProjectsTheRecord(t *testing.T) {
	h := newCreateHarness(t, testBaseDomain)
	rec := seedCreated(t, h, testDBInstanceID)

	out, err := h.svc.DescribeDBInstances(t.Context(), &rds.DescribeDBInstancesInput{
		DBInstanceIdentifier: aws.String(testDBInstanceID),
	}, testAccountID)
	require.NoError(t, err)
	require.Len(t, out.DBInstances, 1)

	instance := out.DBInstances[0]
	assert.Equal(t, testDBInstanceID, aws.StringValue(instance.DBInstanceIdentifier))
	assert.Equal(t, "orders", aws.StringValue(instance.DBName))
	assert.Equal(t, "appuser", aws.StringValue(instance.MasterUsername))
	assert.Equal(t, int64(20), aws.Int64Value(instance.AllocatedStorage))
	assert.Equal(t, storageTypeGP3, aws.StringValue(instance.StorageType))
	assert.True(t, aws.BoolValue(instance.StorageEncrypted))
	assert.Equal(t, rec.EndpointAddress, aws.StringValue(instance.Endpoint.Address))

	// The two guarantees this platform does not make are reported as false rather
	// than left unset, because an absent field reads as "unknown" to a client.
	assert.False(t, aws.BoolValue(instance.MultiAZ))
	assert.False(t, aws.BoolValue(instance.PubliclyAccessible))

	require.NotNil(t, instance.DBSubnetGroup)
	assert.Equal(t, testDefaultVPC, aws.StringValue(instance.DBSubnetGroup.VpcId))
	require.Len(t, instance.VpcSecurityGroups, 1)
	assert.Equal(t, testDefaultSG, aws.StringValue(instance.VpcSecurityGroups[0].VpcSecurityGroupId))
}

// Instances live in per-account buckets, so one account's describe must not see
// another's — the identifier is only unique within an account.
func TestDescribeDBInstances_IsScopedToTheCallingAccount(t *testing.T) {
	h := newCreateHarness(t, "")
	seedCreated(t, h, testDBInstanceID)

	out, err := h.svc.DescribeDBInstances(t.Context(), &rds.DescribeDBInstancesInput{}, "999988887777")
	require.NoError(t, err)
	assert.Empty(t, out.DBInstances)
}

// A DB instance with no endpoint yet has no Endpoint at all: an Endpoint with an
// empty address would have a client dial an empty host.
func TestProjectDBInstance_OmitsAnUnsettledEndpoint(t *testing.T) {
	h := newCreateHarness(t, "")

	instance := h.svc.projectDBInstance(&DBInstanceRecord{
		DBInstanceIdentifier: testDBInstanceID,
		AccountID:            testAccountID,
		Status:               StatusCreating,
		Port:                 5432,
	})
	assert.Nil(t, instance.Endpoint)
	assert.Nil(t, instance.DBName, "a request that named no database has none, not an empty one")
	assert.Nil(t, h.svc.projectDBInstance(nil))
}

func TestDesiredDNSChanges_CoversEveryTenantOrClaimsNoAuthority(t *testing.T) {
	h := newCreateHarness(t, testBaseDomain)
	rec := seedCreated(t, h, testDBInstanceID)

	changes, ok := h.svc.DesiredDNSChanges()
	require.True(t, ok)
	require.Len(t, changes, 1)
	assert.Equal(t, handlers_dns.ActionUpsert, changes[0].Action)
	assert.Equal(t, rec.DNSName, changes[0].Name)
	assert.Equal(t, rec.ENIPrivateIP, changes[0].Value)
}

// Without a base domain there are no managed RDS records, and claiming authority
// would let the reconcile prune records this node cannot even name.
func TestDesiredDNSChanges_ClaimsNoAuthorityWithoutABaseDomain(t *testing.T) {
	h := newCreateHarness(t, "")
	seedCreated(t, h, testDBInstanceID)

	changes, ok := h.svc.DesiredDNSChanges()
	assert.False(t, ok)
	assert.Empty(t, changes)
}

// A deleted instance contributes nothing, so the reconcile prunes its record;
// anything still holding an ENI IP keeps its name resolvable, including a failed
// instance an operator is still investigating.
func TestDesiredDNSChanges_SkipsDeletedButKeepsFailed(t *testing.T) {
	h := newCreateHarness(t, testBaseDomain)
	seedCreated(t, h, "gone-db")
	failed := seedCreated(t, h, "failed-db")

	kv, err := h.svc.bucket(t.Context(), testAccountID)
	require.NoError(t, err)

	gone := h.record(t, "gone-db")
	gone.Status = StatusDeleted
	require.NoError(t, putJSON(t.Context(), kv, DBInstanceKey("gone-db"), &gone))
	failed.Status = StatusFailed
	require.NoError(t, putJSON(t.Context(), kv, DBInstanceKey("failed-db"), &failed))

	changes, ok := h.svc.DesiredDNSChanges()
	require.True(t, ok)
	require.Len(t, changes, 1)
	assert.Equal(t, failed.DNSName, changes[0].Name)
}
