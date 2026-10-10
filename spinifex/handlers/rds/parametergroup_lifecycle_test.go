package handlers_rds

import (
	"encoding/json"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/rds"
	"github.com/mulgadc/spinifex/spinifex/domains/rds/parametergroup"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Pins today's on-the-wire layout: one meta record and one record per stored
// override, each with exactly these JSON field names. A field renamed here
// would be silent for every caller going through the typed record, but not
// for an export, a migration, or a hand-rolled admin tool reading raw KV.
func TestDBParameterGroupRecord_PersistedFieldNames(t *testing.T) {
	t.Parallel()
	h := newCreateHarness(t, testBaseDomain)
	input := parameterGroupInput(testParameterGroup)
	input.Tags = awsTags("env", "prod")
	_, err := h.svc.CreateDBParameterGroup(t.Context(), input, testAccountID)
	require.NoError(t, err)
	_, err = h.svc.ModifyDBParameterGroup(t.Context(), modifyParameters(testParameterGroup,
		parameter("work_mem", "16384", ApplyMethodPendingReboot)), testAccountID)
	require.NoError(t, err)

	kv, err := h.svc.bucket(t.Context(), testAccountID)
	require.NoError(t, err)
	js, err := kv.KV(t.Context())
	require.NoError(t, err)

	metaEntry, err := js.Get(t.Context(), "db-parameter-groups/"+testParameterGroup+"/meta")
	require.NoError(t, err)
	var metaRaw map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(metaEntry.Value(), &metaRaw))
	metaKeys := make([]string, 0, len(metaRaw))
	for k := range metaRaw {
		metaKeys = append(metaKeys, k)
	}
	assert.ElementsMatch(t, []string{
		"name", "accountId", "family", "description", "tags", "createdAt", "updatedAt",
	}, metaKeys)

	paramEntry, err := js.Get(t.Context(), "db-parameter-groups/"+testParameterGroup+"/params/work_mem")
	require.NoError(t, err)
	var paramRaw map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(paramEntry.Value(), &paramRaw))
	paramKeys := make([]string, 0, len(paramRaw))
	for k := range paramRaw {
		paramKeys = append(paramKeys, k)
	}
	assert.ElementsMatch(t, []string{"name", "value", "applyMethod", "updatedAt"}, paramKeys)
}

// A blob written by an older version, or by hand against the documented key
// layout, has to decode to the same values the typed path would have written —
// the KV encoding is a contract on its own, not just an implementation detail
// of createJSON/putJSON.
func TestDBParameterGroupRecord_DecodesAHandWrittenBlob(t *testing.T) {
	t.Parallel()
	h := newCreateHarness(t, testBaseDomain)
	kv, err := h.svc.bucket(t.Context(), testAccountID)
	require.NoError(t, err)
	js, err := kv.KV(t.Context())
	require.NoError(t, err)

	metaJSON := `{
		"name": "tuned-pg",
		"accountId": "` + testAccountID + `",
		"family": "postgres18",
		"description": "Tuned for the orders workload",
		"tags": {"env": "prod"},
		"createdAt": "2024-01-01T00:00:00Z",
		"updatedAt": "2024-01-02T00:00:00Z"
	}`
	_, err = js.Put(t.Context(), "db-parameter-groups/"+testParameterGroup+"/meta", []byte(metaJSON))
	require.NoError(t, err)

	paramJSON := `{
		"name": "work_mem",
		"value": "16384",
		"applyMethod": "pending-reboot",
		"updatedAt": "2024-01-02T00:00:00Z"
	}`
	_, err = js.Put(t.Context(), "db-parameter-groups/"+testParameterGroup+"/params/work_mem", []byte(paramJSON))
	require.NoError(t, err)

	rec, err := h.svc.parameterGroups().Get(t.Context(), testAccountID, testParameterGroup)
	require.NoError(t, err)
	assert.Equal(t, "tuned-pg", rec.Name)
	assert.Equal(t, testAccountID, rec.AccountID)
	assert.Equal(t, "postgres18", rec.Family)
	assert.Equal(t, "Tuned for the orders workload", rec.Description)
	assert.Equal(t, map[string]string{"env": "prod"}, rec.Tags)

	overrides, err := h.svc.parameterGroups().Overrides(t.Context(), testAccountID, testParameterGroup)
	require.NoError(t, err)
	override, ok := overrides["work_mem"]
	require.True(t, ok)
	assert.Equal(t, "16384", override.Value)
	assert.Equal(t, ApplyMethodPendingReboot, override.ApplyMethod)
}

// The default group is synthesised on every read rather than materialised by
// the first one: a describe of it must not leave a meta record behind for a
// later create of the same name to collide with or inherit.
func TestDescribeDBParameterGroups_TheDefaultGroupIsSynthesisedNotStored(t *testing.T) {
	t.Parallel()
	h := newCreateHarness(t, testBaseDomain)

	out, err := h.svc.DescribeDBParameterGroups(t.Context(),
		&rds.DescribeDBParameterGroupsInput{DBParameterGroupName: aws.String(testDefaultPG)}, testAccountID)
	require.NoError(t, err)
	require.Len(t, out.DBParameterGroups, 1)
	group := out.DBParameterGroups[0]
	assert.Equal(t, "postgres18", aws.StringValue(group.DBParameterGroupFamily))
	assert.Equal(t, "Default parameter group for postgres18", aws.StringValue(group.Description))

	kv, err := h.svc.bucket(t.Context(), testAccountID)
	require.NoError(t, err)
	var rec parametergroup.Record
	found, err := getJSON(t.Context(), kv, parametergroup.MetaKey(testDefaultPG), &rec)
	require.NoError(t, err)
	assert.False(t, found, "a lazily-synthesised default group must not be written by a describe")
}

// A second delete of the same name finds nothing rather than a group that
// somehow persisted, and reports it with the ordinary not-found code.
func TestDeleteDBParameterGroup_ASecondDeleteReturnsNotFound(t *testing.T) {
	t.Parallel()
	h := newCreateHarness(t, testBaseDomain)
	_, err := h.svc.CreateDBParameterGroup(t.Context(), parameterGroupInput(testParameterGroup), testAccountID)
	require.NoError(t, err)

	_, err = h.svc.DeleteDBParameterGroup(t.Context(),
		&rds.DeleteDBParameterGroupInput{DBParameterGroupName: aws.String(testParameterGroup)}, testAccountID)
	require.NoError(t, err)

	_, err = h.svc.DeleteDBParameterGroup(t.Context(),
		&rds.DeleteDBParameterGroupInput{DBParameterGroupName: aws.String(testParameterGroup)}, testAccountID)
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorDBParameterGroupNotFound, awserrors.ValidErrorCodeFromError(err),
		"the code has to survive resolution or the client sees a 500")
}

// An instance that only names the group in a pending attachment is not yet
// using it, but the delete refuses anyway: it has to leave the group in place
// for the modify that is still going to need it.
func TestDeleteDBParameterGroup_RefusesWhileOnlyPendingAttached(t *testing.T) {
	t.Parallel()
	h := newCreateHarness(t, testBaseDomain)
	_, err := h.svc.CreateDBParameterGroup(t.Context(), parameterGroupInput(testParameterGroup), testAccountID)
	require.NoError(t, err)

	rec := defaultRecord()
	rec.Status = StatusModifying
	rec.DBParameterGroupName = testDefaultPG
	rec.PendingModifiedValues = &PendingModifiedValues{DBParameterGroupName: testParameterGroup}
	seedInstance(t, h.svc, rec)

	_, err = h.svc.DeleteDBParameterGroup(t.Context(),
		&rds.DeleteDBParameterGroupInput{DBParameterGroupName: aws.String(testParameterGroup)}, testAccountID)
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorDBParameterGroupInvalidState, awserrors.ValidErrorCodeFromError(err),
		"the code has to survive resolution or the client sees a 500")
	assert.Contains(t, err.Error(), testDBID)
}

// AWS defers a pending-reboot request until the next reboot, even for a
// dynamic parameter. Present behaviour does not: propagation sends the whole
// resolved set to the agent regardless of the ApplyMethod the customer asked
// for, and it is the agent, not the stored method, that decides what lands
// live versus what waits for a restart.
func TestModifyDBParameterGroup_SendsADynamicPendingRebootParameterImmediately(t *testing.T) {
	t.Parallel()
	h := newModifyHarness(t)
	_, err := h.svc.CreateDBParameterGroup(t.Context(), parameterGroupInput(testParameterGroup), testAccountID)
	require.NoError(t, err)

	rec := modifiableRecord()
	rec.DBParameterGroupName = testParameterGroup
	seedInstance(t, h.svc, rec)

	_, err = h.svc.ModifyDBParameterGroup(t.Context(), modifyParameters(testParameterGroup,
		parameter("work_mem", "16384", ApplyMethodPendingReboot),
	), testAccountID)
	require.NoError(t, err)

	out, err := h.svc.DescribeDBParameters(t.Context(),
		&rds.DescribeDBParametersInput{DBParameterGroupName: aws.String(testParameterGroup)}, testAccountID)
	require.NoError(t, err)
	var workMem *rds.Parameter
	for _, p := range out.Parameters {
		if aws.StringValue(p.ParameterName) == "work_mem" {
			workMem = p
		}
	}
	require.NotNil(t, workMem)
	assert.Equal(t, ApplyMethodPendingReboot, aws.StringValue(workMem.ApplyMethod),
		"the stored request is reported back as pending-reboot even though work_mem is dynamic")

	issued := h.agent.received()
	require.Len(t, issued, 1)
	assert.Equal(t, CommandApplyParams, issued[0].Type)
	assert.Contains(t, issued[0].Parameters, Parameter{Name: "work_mem", Value: "16384"},
		"present behaviour: the full resolved set is sent in the apply command regardless of ApplyMethod")

	stored := h.record(t)
	assert.Empty(t, stored.PendingRebootParameters,
		"the stub agent's reply, not the stored ApplyMethod, is what decides this is empty")
}

// AWS would not push a live apply-params command at a stopped instance.
// Present propagation does not consult the instance's Status at all before
// sending, so a stopped instance still receives (and here, still answers) it.
func TestModifyDBParameterGroup_PropagatesToAStoppedInstance(t *testing.T) {
	t.Parallel()
	h := newModifyHarness(t)
	_, err := h.svc.CreateDBParameterGroup(t.Context(), parameterGroupInput(testParameterGroup), testAccountID)
	require.NoError(t, err)

	rec := modifiableRecord()
	rec.DBParameterGroupName = testParameterGroup
	rec.Status = StatusStopped
	seedInstance(t, h.svc, rec)

	_, err = h.svc.ModifyDBParameterGroup(t.Context(), modifyParameters(testParameterGroup,
		parameter("work_mem", "16384", ApplyMethodImmediate),
	), testAccountID)
	require.NoError(t, err)

	issued := h.agent.received()
	require.Len(t, issued, 1, "present behaviour: propagation does not check instance status before sending")
	assert.Equal(t, CommandApplyParams, issued[0].Type)
	assert.Contains(t, issued[0].Parameters, Parameter{Name: "work_mem", Value: "16384"})

	stored := storedDBInstance(t, h.svc, testDBID)
	assert.Equal(t, StatusStopped, stored.Status, "propagation does not change the instance's status")
	assert.False(t, stored.ParameterApplyFailed)
}
