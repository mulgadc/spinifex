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

// The Terraform provider keys its state off DbiResourceId rather than off the
// identifier, so an instance created without one is unmanageable by the tool
// this service is driven with — its post-create read finds nothing.
func TestDescribeDBInstances_ReportsAStableResourceID(t *testing.T) {
	h := newCreateHarness(t, "")
	rec := seedCreated(t, h, testDBInstanceID)
	require.Regexp(t, `^db-[0-9a-f]{17}$`, rec.DbiResourceID)

	other := seedCreated(t, h, "other-db")
	assert.NotEqual(t, rec.DbiResourceID, other.DbiResourceID, "the handle is per instance")

	out, err := h.svc.DescribeDBInstances(t.Context(), &rds.DescribeDBInstancesInput{
		DBInstanceIdentifier: aws.String(testDBInstanceID),
	}, testAccountID)
	require.NoError(t, err)
	require.Len(t, out.DBInstances, 1)
	assert.Equal(t, rec.DbiResourceID, aws.StringValue(out.DBInstances[0].DbiResourceId))
}

// The read half of the same path: the provider looks an instance up by filtering
// on the resource ID, and a filter that is parsed but not applied answers with
// every instance in the account instead of the one asked for.
func TestDescribeDBInstances_FiltersOnResourceIDAndIdentifier(t *testing.T) {
	h := newCreateHarness(t, "")
	want := seedCreated(t, h, testDBInstanceID)
	seedCreated(t, h, "other-db")

	for name, filter := range map[string]*rds.Filter{
		filterDbiResourceID: {Name: aws.String(filterDbiResourceID), Values: aws.StringSlice([]string{want.DbiResourceID})},
		filterDBInstanceID:  {Name: aws.String(filterDBInstanceID), Values: aws.StringSlice([]string{testDBInstanceID})},
	} {
		t.Run(name, func(t *testing.T) {
			out, err := h.svc.DescribeDBInstances(t.Context(), &rds.DescribeDBInstancesInput{
				Filters: []*rds.Filter{filter},
			}, testAccountID)
			require.NoError(t, err)
			require.Len(t, out.DBInstances, 1)
			assert.Equal(t, testDBInstanceID, aws.StringValue(out.DBInstances[0].DBInstanceIdentifier))
		})
	}
}

// Nothing acts on AutoMinorVersionUpgrade — the version is pinned — but AWS and
// the Terraform provider both default it to true, so a describe that reported
// false would leave every default configuration with a diff that no modify can
// clear.
func TestDescribeDBInstances_EchoesAutoMinorVersionUpgrade(t *testing.T) {
	h := newCreateHarness(t, "")

	unset := seedCreated(t, h, testDBInstanceID)
	assert.True(t, unset.AutoMinorVersionUpgrade, "an unset request takes AWS's own default")

	input := validCreateInput()
	input.DBInstanceIdentifier = aws.String("opted-out-db")
	input.AutoMinorVersionUpgrade = aws.Bool(false)
	_, err := h.svc.CreateDBInstance(t.Context(), input, testAccountID)
	require.NoError(t, err)

	out, err := h.svc.DescribeDBInstances(t.Context(), &rds.DescribeDBInstancesInput{
		DBInstanceIdentifier: aws.String("opted-out-db"),
	}, testAccountID)
	require.NoError(t, err)
	require.Len(t, out.DBInstances, 1)
	assert.False(t, aws.BoolValue(out.DBInstances[0].AutoMinorVersionUpgrade))
}

func TestDescribeDBInstances_EchoesAcceptedInertSettings(t *testing.T) {
	h := newCreateHarness(t, "")
	input := validCreateInput()
	input.CopyTagsToSnapshot = aws.Bool(true)
	input.EnablePerformanceInsights = aws.Bool(true)
	input.MonitoringInterval = aws.Int64(60)

	_, err := h.svc.CreateDBInstance(t.Context(), input, testAccountID)
	require.NoError(t, err)

	rec := h.record(t, testDBInstanceID)
	assert.True(t, rec.CopyTagsToSnapshot)
	assert.True(t, rec.EnablePerformanceInsights)
	assert.Equal(t, int64(60), rec.MonitoringInterval)

	out, err := h.svc.DescribeDBInstances(t.Context(), &rds.DescribeDBInstancesInput{
		DBInstanceIdentifier: aws.String(testDBInstanceID),
	}, testAccountID)
	require.NoError(t, err)
	require.Len(t, out.DBInstances, 1)
	assert.True(t, aws.BoolValue(out.DBInstances[0].CopyTagsToSnapshot))
	assert.True(t, aws.BoolValue(out.DBInstances[0].PerformanceInsightsEnabled))
	assert.Equal(t, int64(60), aws.Int64Value(out.DBInstances[0].MonitoringInterval))
}

func TestDescribeDBInstances_EchoesOmittedInertSettingsAsDisabled(t *testing.T) {
	h := newCreateHarness(t, "")
	seedCreated(t, h, testDBInstanceID)

	out, err := h.svc.DescribeDBInstances(t.Context(), &rds.DescribeDBInstancesInput{
		DBInstanceIdentifier: aws.String(testDBInstanceID),
	}, testAccountID)
	require.NoError(t, err)
	require.Len(t, out.DBInstances, 1)
	assert.False(t, aws.BoolValue(out.DBInstances[0].CopyTagsToSnapshot))
	assert.False(t, aws.BoolValue(out.DBInstances[0].PerformanceInsightsEnabled))
	assert.Zero(t, aws.Int64Value(out.DBInstances[0].MonitoringInterval))
}

// A filter nobody implemented is refused rather than dropped: dropping it
// returns exactly the rows the caller asked to exclude.
func TestDescribeDBInstances_RejectsAnUnrecognizedFilter(t *testing.T) {
	h := newCreateHarness(t, "")
	seedCreated(t, h, testDBInstanceID)

	_, err := h.svc.DescribeDBInstances(t.Context(), &rds.DescribeDBInstancesInput{
		Filters: []*rds.Filter{{Name: aws.String("engine"), Values: aws.StringSlice([]string{"postgres"})}},
	}, testAccountID)
	require.Error(t, err)
	assert.Contains(t, err.Error(), awserrors.ErrorInvalidParameterValue)
}

// A named instance and a filter that excludes it disagree; reporting the
// instance anyway would answer a question the caller did not ask.
func TestDescribeDBInstances_NamedInstanceMustSatisfyTheFilters(t *testing.T) {
	h := newCreateHarness(t, "")
	seedCreated(t, h, testDBInstanceID)

	_, err := h.svc.DescribeDBInstances(t.Context(), &rds.DescribeDBInstancesInput{
		DBInstanceIdentifier: aws.String(testDBInstanceID),
		Filters: []*rds.Filter{{
			Name: aws.String(filterDbiResourceID), Values: aws.StringSlice([]string{"db-000000000000000ff"}),
		}},
	}, testAccountID)
	require.Error(t, err)
	assert.Contains(t, err.Error(), awserrors.ErrorDBInstanceNotFound)
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
	assert.Equal(t, int64(5432), aws.Int64Value(instance.DbInstancePort))
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

// A deferred change is what the customer polls for, and the Terraform provider
// reads PendingModifiedValues to decide whether an apply has landed.
func TestProjectDBInstance_ReportsTheDeferredChange(t *testing.T) {
	h := newCreateHarness(t, "")

	instance := h.svc.projectDBInstance(&DBInstanceRecord{
		DBInstanceIdentifier: testDBInstanceID,
		Status:               StatusAvailable,
		DBInstanceClass:      "db.t3.medium",
		AllocatedStorage:     20,
		PendingModifiedValues: &PendingModifiedValues{
			AllocatedStorage: aws.Int64(50),
			DBInstanceClass:  "db.m5.large",
		},
	})

	require.NotNil(t, instance.PendingModifiedValues)
	assert.Equal(t, int64(50), aws.Int64Value(instance.PendingModifiedValues.AllocatedStorage))
	assert.Equal(t, "db.m5.large", aws.StringValue(instance.PendingModifiedValues.DBInstanceClass))
	// The current values still report what is actually in effect.
	assert.Equal(t, int64(20), aws.Int64Value(instance.AllocatedStorage))
	assert.Equal(t, "db.t3.medium", aws.StringValue(instance.DBInstanceClass))
}

// The volume is already at the new size once only the in-guest grow is left,
// so reporting it as still pending would show Terraform drift on a change that
// has landed.
func TestProjectDBInstance_OmitsAPendingFilesystemGrow(t *testing.T) {
	h := newCreateHarness(t, "")

	instance := h.svc.projectDBInstance(&DBInstanceRecord{
		DBInstanceIdentifier:  testDBInstanceID,
		Status:                StatusModifying,
		AllocatedStorage:      50,
		PendingModifiedValues: &PendingModifiedValues{FilesystemGrowPending: true},
	})
	assert.Nil(t, instance.PendingModifiedValues)
}

// AWS reports a parameter group's state on the membership rather than in
// PendingModifiedValues, and the Terraform provider reads it there.
func TestProjectDBInstance_ReportsTheParameterGroupApplyStatus(t *testing.T) {
	h := newCreateHarness(t, "")
	rec := &DBInstanceRecord{
		DBInstanceIdentifier: testDBInstanceID,
		Status:               StatusAvailable,
		DBParameterGroupName: "default.postgres18",
	}

	statusOf := func(rec *DBInstanceRecord) string {
		groups := h.svc.projectDBInstance(rec).DBParameterGroups
		require.Len(t, groups, 1)
		assert.Equal(t, "default.postgres18", aws.StringValue(groups[0].DBParameterGroupName))
		return aws.StringValue(groups[0].ParameterApplyStatus)
	}

	assert.Equal(t, "in-sync", statusOf(rec))

	// Static settings are in the engine's config but not in effect until
	// the restart that adopts them.
	rec.PendingRebootParameters = []string{"shared_buffers"}
	assert.Equal(t, "pending-reboot", statusOf(rec))

	rec.PendingModifiedValues = &PendingModifiedValues{DBParameterGroupName: "default.postgres18"}
	assert.Equal(t, "applying", statusOf(rec))
}
