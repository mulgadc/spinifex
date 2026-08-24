package handlers_rds

import (
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/rds"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	testParameterGroup = "tuned-pg"
	testDefaultPG      = "default.postgres18"
	testDefaultMariaDB = "default.mariadb11.8"
)

func parameterGroupInput(name string) *rds.CreateDBParameterGroupInput {
	return &rds.CreateDBParameterGroupInput{
		DBParameterGroupName:   aws.String(name),
		DBParameterGroupFamily: aws.String("postgres18"),
		Description:            aws.String("Tuned for the orders workload"),
	}
}

func modifyParameters(name string, params ...*rds.Parameter) *rds.ModifyDBParameterGroupInput {
	return &rds.ModifyDBParameterGroupInput{
		DBParameterGroupName: aws.String(name),
		Parameters:           params,
	}
}

func parameter(name, value, applyMethod string) *rds.Parameter {
	p := &rds.Parameter{ParameterName: aws.String(name), ParameterValue: aws.String(value)}
	if applyMethod != "" {
		p.ApplyMethod = aws.String(applyMethod)
	}
	return p
}

func storedDBInstance(t *testing.T, svc *Service, id string) DBInstanceRecord {
	t.Helper()
	kv, err := svc.bucket(t.Context(), testAccountID)
	require.NoError(t, err)
	var rec DBInstanceRecord
	found, err := getJSON(t.Context(), kv, DBInstanceKey(id), &rec)
	require.NoError(t, err)
	require.True(t, found)
	return rec
}

// The values of a described group, keyed by name, with the source each was
// reported under.
func describedParameters(t *testing.T, h *createHarness, group string) map[string]*rds.Parameter {
	t.Helper()
	out, err := h.svc.DescribeDBParameters(t.Context(),
		&rds.DescribeDBParametersInput{DBParameterGroupName: aws.String(group)}, testAccountID)
	require.NoError(t, err)

	byName := make(map[string]*rds.Parameter, len(out.Parameters))
	for _, param := range out.Parameters {
		byName[aws.StringValue(param.ParameterName)] = param
	}
	return byName
}

func TestCreateDBParameterGroup_StoresAnEmptyGroup(t *testing.T) {
	t.Parallel()
	h := newCreateHarness(t, testBaseDomain)

	out, err := h.svc.CreateDBParameterGroup(t.Context(), parameterGroupInput(testParameterGroup), testAccountID)
	require.NoError(t, err)

	group := out.DBParameterGroup
	require.NotNil(t, group)
	assert.Equal(t, testParameterGroup, aws.StringValue(group.DBParameterGroupName))
	assert.Equal(t, "postgres18", aws.StringValue(group.DBParameterGroupFamily))
	assert.Equal(t, FormatARN(ResourceKindDBParameterGroup, testRegion, testAccountID, testParameterGroup),
		aws.StringValue(group.DBParameterGroupArn))

	// A fresh group and the default group resolve to the same effective set,
	// because a group holds overrides rather than a copy of the catalog.
	params := describedParameters(t, h, testParameterGroup)
	require.Len(t, params, len(enginePostgres.CatalogParameterNames()))
	for _, param := range params {
		assert.Equal(t, ParameterSourceEngineDefault, aws.StringValue(param.Source))
	}
}

// An omitted family can only mean the one family this platform offers, so it
// takes the pin rather than failing a Terraform config that leaves it out.
func TestCreateDBParameterGroup_DefaultsTheFamilyAndRejectsAnother(t *testing.T) {
	t.Parallel()
	h := newCreateHarness(t, testBaseDomain)

	input := parameterGroupInput(testParameterGroup)
	input.DBParameterGroupFamily = nil
	out, err := h.svc.CreateDBParameterGroup(t.Context(), input, testAccountID)
	require.NoError(t, err)
	assert.Equal(t, "postgres18", aws.StringValue(out.DBParameterGroup.DBParameterGroupFamily))

	other := parameterGroupInput("mysql-tuned")
	other.DBParameterGroupFamily = aws.String("mysql8.0")
	_, err = h.svc.CreateDBParameterGroup(t.Context(), other, testAccountID)
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorInvalidParameterValue, awserrors.ValidErrorCodeFromError(err),
		"the code has to survive resolution or the client sees a 500")
}

// A customer group under the reserved prefix would be indistinguishable from the
// implicit one, and would then be modifiable through a name that must not be.
func TestCreateDBParameterGroup_RejectsTheReservedPrefix(t *testing.T) {
	t.Parallel()
	h := newCreateHarness(t, testBaseDomain)

	_, err := h.svc.CreateDBParameterGroup(t.Context(), parameterGroupInput("default.postgres18"), testAccountID)
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorInvalidParameterValue, awserrors.ValidErrorCodeFromError(err),
		"the code has to survive resolution or the client sees a 500")
	assert.Contains(t, err.Error(), "default.")
}

func TestCreateDBParameterGroup_RejectsADuplicateName(t *testing.T) {
	t.Parallel()
	h := newCreateHarness(t, testBaseDomain)
	_, err := h.svc.CreateDBParameterGroup(t.Context(), parameterGroupInput(testParameterGroup), testAccountID)
	require.NoError(t, err)

	_, err = h.svc.CreateDBParameterGroup(t.Context(), parameterGroupInput(testParameterGroup), testAccountID)
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorDBParameterGroupAlreadyExists, awserrors.ValidErrorCodeFromError(err),
		"the code has to survive resolution or the client sees a 500")
}

// The default group is what CreateDBInstance resolves when a request names none,
// so it has to be listable and readable without anyone having created it.
func TestDescribeDBParameterGroups_ReportsTheImplicitDefault(t *testing.T) {
	t.Parallel()
	h := newCreateHarness(t, testBaseDomain)

	named, err := h.svc.DescribeDBParameterGroups(t.Context(),
		&rds.DescribeDBParameterGroupsInput{DBParameterGroupName: aws.String(testDefaultPG)}, testAccountID)
	require.NoError(t, err)
	require.Len(t, named.DBParameterGroups, 1)
	assert.Equal(t, testDefaultPG, aws.StringValue(named.DBParameterGroups[0].DBParameterGroupName))

	// One per registered engine, as AWS does: an account that has never touched
	// MariaDB still sees its default group, because the group is the engine's.
	listed, err := h.svc.DescribeDBParameterGroups(t.Context(), &rds.DescribeDBParameterGroupsInput{}, testAccountID)
	require.NoError(t, err)
	assert.Equal(t, []string{testDefaultMariaDB, testDefaultPG}, parameterGroupNames(listed),
		"the default groups are reported even with nothing created")
}

func parameterGroupNames(out *rds.DescribeDBParameterGroupsOutput) []string {
	var names []string
	for _, group := range out.DBParameterGroups {
		names = append(names, aws.StringValue(group.DBParameterGroupName))
	}
	return names
}

// Synthesised rather than stored, so it must appear exactly once alongside the
// customer's own groups rather than twice or not at all.
func TestDescribeDBParameterGroups_ListsCustomerGroupsBesideTheDefault(t *testing.T) {
	t.Parallel()
	h := newCreateHarness(t, testBaseDomain)
	_, err := h.svc.CreateDBParameterGroup(t.Context(), parameterGroupInput("zeta"), testAccountID)
	require.NoError(t, err)
	_, err = h.svc.CreateDBParameterGroup(t.Context(), parameterGroupInput("alpha"), testAccountID)
	require.NoError(t, err)

	listed, err := h.svc.DescribeDBParameterGroups(t.Context(), &rds.DescribeDBParameterGroupsInput{}, testAccountID)
	require.NoError(t, err)
	assert.Equal(t, []string{"alpha", testDefaultMariaDB, testDefaultPG, "zeta"}, parameterGroupNames(listed))
}

func TestDescribeDBParameterGroups_RejectsAnUnknownName(t *testing.T) {
	t.Parallel()
	h := newCreateHarness(t, testBaseDomain)

	_, err := h.svc.DescribeDBParameterGroups(t.Context(),
		&rds.DescribeDBParameterGroupsInput{DBParameterGroupName: aws.String("absent")}, testAccountID)
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorDBParameterGroupNotFound, awserrors.ValidErrorCodeFromError(err),
		"the code has to survive resolution or the client sees a 500")

	// A default.* name for an engine that does not exist is a not-found too,
	// rather than a group that resolves to nothing.
	_, err = h.svc.DescribeDBParameterGroups(t.Context(),
		&rds.DescribeDBParameterGroupsInput{DBParameterGroupName: aws.String("default.mysql8")}, testAccountID)
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorDBParameterGroupNotFound, awserrors.ValidErrorCodeFromError(err),
		"the code has to survive resolution or the client sees a 500")
}

func TestModifyDBParameterGroup_StoresValidatedOverrides(t *testing.T) {
	t.Parallel()
	h := newCreateHarness(t, testBaseDomain)
	_, err := h.svc.CreateDBParameterGroup(t.Context(), parameterGroupInput(testParameterGroup), testAccountID)
	require.NoError(t, err)

	out, err := h.svc.ModifyDBParameterGroup(t.Context(), modifyParameters(testParameterGroup,
		parameter("work_mem", "16384", ApplyMethodPendingReboot),
		parameter("shared_buffers", "65536", ApplyMethodPendingReboot),
	), testAccountID)
	require.NoError(t, err)
	assert.Equal(t, testParameterGroup, aws.StringValue(out.DBParameterGroupName))

	params := describedParameters(t, h, testParameterGroup)
	assert.Equal(t, "16384", aws.StringValue(params["work_mem"].ParameterValue))
	assert.Equal(t, ParameterSourceUser, aws.StringValue(params["work_mem"].Source))
	assert.Equal(t, ApplyMethodPendingReboot, aws.StringValue(params["work_mem"].ApplyMethod),
		"a dynamic parameter must report the customer's stored method")
	assert.Equal(t, "65536", aws.StringValue(params["shared_buffers"].ParameterValue))
	assert.Equal(t, ParameterSourceUser, aws.StringValue(params["shared_buffers"].Source))
	// Untouched parameters stay engine defaults rather than becoming user values.
	assert.Equal(t, ParameterSourceEngineDefault, aws.StringValue(params["autovacuum"].Source))
}

func TestModifyDBParameterGroup_PropagatesDynamicParametersToEveryAttachedInstance(t *testing.T) {
	t.Parallel()
	h := newModifyHarness(t)
	_, err := h.svc.CreateDBParameterGroup(t.Context(), parameterGroupInput(testParameterGroup), testAccountID)
	require.NoError(t, err)

	first := modifiableRecord()
	first.DBParameterGroupName = testParameterGroup
	seedInstance(t, h.svc, first)

	secondID := testDBID + "-second"
	second := modifiableRecord()
	second.DBInstanceIdentifier = secondID
	second.DBParameterGroupName = testParameterGroup
	seedInstance(t, h.svc, second)
	secondAgent := newStubAgent(t, h.nc, testAccountID, secondID, false)

	_, err = h.svc.ModifyDBParameterGroup(t.Context(), modifyParameters(testParameterGroup,
		parameter("work_mem", "16384", ApplyMethodImmediate),
	), testAccountID)
	require.NoError(t, err)

	for id, issued := range map[string][]Command{
		testDBID: h.agent.received(),
		secondID: secondAgent.received(),
	} {
		require.Len(t, issued, 1, "instance %s must receive the group update", id)
		assert.Equal(t, CommandApplyParams, issued[0].Type)
		assert.Contains(t, issued[0].Parameters, Parameter{Name: "work_mem", Value: "16384"})

		stored := storedDBInstance(t, h.svc, id)
		assert.Contains(t, stored.Bootstrap.ResolvedParameters, Parameter{Name: "work_mem", Value: "16384"})
		assert.Empty(t, stored.PendingRebootParameters)
		groups := projectParameterGroup(&stored)
		require.Len(t, groups, 1)
		assert.Equal(t, "in-sync", aws.StringValue(groups[0].ParameterApplyStatus))
	}
}

// The override is stored before it is propagated, so a guest that refuses it
// leaves the group holding a value the engine never adopted. That has to reach
// the instance's apply status rather than being reported as in-sync.
func TestModifyDBParameterGroup_RecordsAFailedApplyOnTheInstance(t *testing.T) {
	t.Parallel()
	h := newModifyHarnessWithAgent(t, true)
	_, err := h.svc.CreateDBParameterGroup(t.Context(), parameterGroupInput(testParameterGroup), testAccountID)
	require.NoError(t, err)

	rec := modifiableRecord()
	rec.DBParameterGroupName = testParameterGroup
	seedInstance(t, h.svc, rec)

	_, err = h.svc.ModifyDBParameterGroup(t.Context(), modifyParameters(testParameterGroup,
		parameter("work_mem", "16384", ApplyMethodImmediate),
	), testAccountID)
	require.Error(t, err)

	stored := h.record(t)
	assert.True(t, stored.ParameterApplyFailed)
	groups := projectParameterGroup(&stored)
	require.Len(t, groups, 1)
	assert.Equal(t, "failed-to-apply", aws.StringValue(groups[0].ParameterApplyStatus))
	assert.Contains(t, strings.Join(h.eventMessages(t), "\n"), "could not be applied")
}

// A later apply the engine accepts is what clears it, so the instance does not
// keep reporting a failure it has recovered from.
func TestModifyDBParameterGroup_ASuccessfulApplyClearsTheRecordedFailure(t *testing.T) {
	t.Parallel()
	h := newModifyHarness(t)
	_, err := h.svc.CreateDBParameterGroup(t.Context(), parameterGroupInput(testParameterGroup), testAccountID)
	require.NoError(t, err)

	rec := modifiableRecord()
	rec.DBParameterGroupName = testParameterGroup
	rec.ParameterApplyFailed = true
	seedInstance(t, h.svc, rec)

	_, err = h.svc.ModifyDBParameterGroup(t.Context(), modifyParameters(testParameterGroup,
		parameter("work_mem", "16384", ApplyMethodImmediate),
	), testAccountID)
	require.NoError(t, err)

	stored := h.record(t)
	assert.False(t, stored.ParameterApplyFailed)
	groups := projectParameterGroup(&stored)
	require.Len(t, groups, 1)
	assert.Equal(t, "in-sync", aws.StringValue(groups[0].ParameterApplyStatus))
}

// Propagation marks the instances that refused the set, not the group: an
// instance that took the same values stays in-sync.
func TestModifyDBParameterGroup_MarksOnlyTheInstancesWhoseApplyFailed(t *testing.T) {
	t.Parallel()
	h := newModifyHarnessWithAgent(t, true)
	_, err := h.svc.CreateDBParameterGroup(t.Context(), parameterGroupInput(testParameterGroup), testAccountID)
	require.NoError(t, err)

	refusing := modifiableRecord()
	refusing.DBParameterGroupName = testParameterGroup
	seedInstance(t, h.svc, refusing)

	acceptingID := testDBID + "-second"
	accepting := modifiableRecord()
	accepting.DBInstanceIdentifier = acceptingID
	accepting.DBParameterGroupName = testParameterGroup
	seedInstance(t, h.svc, accepting)
	newStubAgent(t, h.nc, testAccountID, acceptingID, false)

	_, err = h.svc.ModifyDBParameterGroup(t.Context(), modifyParameters(testParameterGroup,
		parameter("work_mem", "16384", ApplyMethodImmediate),
	), testAccountID)
	require.Error(t, err)

	assert.True(t, storedDBInstance(t, h.svc, testDBID).ParameterApplyFailed)

	stored := storedDBInstance(t, h.svc, acceptingID)
	assert.False(t, stored.ParameterApplyFailed)
	groups := projectParameterGroup(&stored)
	require.Len(t, groups, 1)
	assert.Equal(t, "in-sync", aws.StringValue(groups[0].ParameterApplyStatus))
}

func TestModifyDBParameterGroup_RecordsStaticParametersPendingReboot(t *testing.T) {
	t.Parallel()
	h := newModifyHarness(t)
	h.agent.replyWith("max_connections")
	_, err := h.svc.CreateDBParameterGroup(t.Context(), parameterGroupInput(testParameterGroup), testAccountID)
	require.NoError(t, err)

	rec := modifiableRecord()
	rec.DBParameterGroupName = testParameterGroup
	seedInstance(t, h.svc, rec)

	_, err = h.svc.ModifyDBParameterGroup(t.Context(), modifyParameters(testParameterGroup,
		parameter("max_connections", "137", ApplyMethodPendingReboot),
	), testAccountID)
	require.NoError(t, err)

	stored := h.record(t)
	assert.Equal(t, []string{"max_connections"}, stored.PendingRebootParameters)
	groups := projectParameterGroup(&stored)
	require.Len(t, groups, 1)
	assert.Equal(t, "pending-reboot", aws.StringValue(groups[0].ParameterApplyStatus))
}

func TestModifyDBParameterGroup_DoesNotPropagateToAPendingAttachment(t *testing.T) {
	t.Parallel()
	h := newModifyHarness(t)
	_, err := h.svc.CreateDBParameterGroup(t.Context(), parameterGroupInput(testParameterGroup), testAccountID)
	require.NoError(t, err)

	rec := modifiableRecord()
	rec.PendingModifiedValues = &PendingModifiedValues{DBParameterGroupName: testParameterGroup}
	seedInstance(t, h.svc, rec)

	_, err = h.svc.ModifyDBParameterGroup(t.Context(), modifyParameters(testParameterGroup,
		parameter("work_mem", "16384", ApplyMethodImmediate),
	), testAccountID)
	require.NoError(t, err)
	assert.Empty(t, h.agent.received(), "the latest values resolve when the pending attachment is applied")
}

func TestModifyDBParameterGroup_ReturnsAPropagationFailure(t *testing.T) {
	t.Parallel()
	h := newModifyHarnessWithAgent(t, true)
	_, err := h.svc.CreateDBParameterGroup(t.Context(), parameterGroupInput(testParameterGroup), testAccountID)
	require.NoError(t, err)

	rec := modifiableRecord()
	rec.DBParameterGroupName = testParameterGroup
	seedInstance(t, h.svc, rec)

	_, err = h.svc.ModifyDBParameterGroup(t.Context(), modifyParameters(testParameterGroup,
		parameter("work_mem", "16384", ApplyMethodImmediate),
	), testAccountID)
	require.Error(t, err)
	assert.Contains(t, err.Error(), testDBID)

	overrides, listErr := ListDBParameterOverrides(t.Context(), h.kv(t), testParameterGroup)
	require.NoError(t, listErr)
	assert.Equal(t, "16384", overrides["work_mem"].Value,
		"the durable group edit remains available for a retry")
	assert.NotContains(t, h.record(t).Bootstrap.ResolvedParameters,
		Parameter{Name: "work_mem", Value: "16384"}, "a failed apply must not advance the instance record")
}

// A batch with one bad value must leave the group exactly as it was: a
// half-applied modify would be a configuration nobody asked for.
func TestModifyDBParameterGroup_WritesNothingWhenOneValueIsBad(t *testing.T) {
	t.Parallel()
	h := newCreateHarness(t, testBaseDomain)
	_, err := h.svc.CreateDBParameterGroup(t.Context(), parameterGroupInput(testParameterGroup), testAccountID)
	require.NoError(t, err)

	_, err = h.svc.ModifyDBParameterGroup(t.Context(), modifyParameters(testParameterGroup,
		parameter("work_mem", "16384", ""),
		parameter("max_connections", "999999", ApplyMethodPendingReboot),
	), testAccountID)
	require.Error(t, err)

	params := describedParameters(t, h, testParameterGroup)
	assert.Equal(t, ParameterSourceEngineDefault, aws.StringValue(params["work_mem"].Source),
		"the valid half of a rejected batch must not have landed")
}

func TestModifyDBParameterGroup_RejectsBadRequests(t *testing.T) {
	cases := []struct {
		name  string
		input *rds.ModifyDBParameterGroupInput
		want  string
	}{
		{"UnknownParameter", modifyParameters(testParameterGroup, parameter("not_a_setting", "1", "")),
			"is not a parameter this engine exposes"},
		{"OutOfRange", modifyParameters(testParameterGroup, parameter("max_connections", "999999", "")),
			"outside its allowed range"},
		{"Formula", modifyParameters(testParameterGroup, parameter("shared_buffers", "{DBInstanceClassMemory/32768}", "")),
			"is a formula"},
		// Telling the customer a static change is live when the engine has not
		// adopted it is worse than rejecting the request.
		{"ImmediateOnAStaticParameter", modifyParameters(testParameterGroup, parameter("shared_buffers", "65536", ApplyMethodImmediate)),
			"is static"},
		{"UnknownApplyMethod", modifyParameters(testParameterGroup, parameter("work_mem", "16384", "eventually")),
			"ApplyMethod"},
		{"NoParameters", modifyParameters(testParameterGroup), "at least one parameter"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newCreateHarness(t, testBaseDomain)
			_, err := h.svc.CreateDBParameterGroup(t.Context(), parameterGroupInput(testParameterGroup), testAccountID)
			require.NoError(t, err)

			_, err = h.svc.ModifyDBParameterGroup(t.Context(), tc.input, testAccountID)
			require.Error(t, err)
			assert.Equal(t, awserrors.ErrorInvalidParameterValue, awserrors.ValidErrorCodeFromError(err),
				"the code has to survive resolution or the client sees a 500")
			assert.Contains(t, err.Error(), tc.want)
		})
	}
}

// AWS's own rule, and the one that keeps the default group a stable reference:
// an instance that names it must get the catalog's values, not a tenant's edits.
func TestModifyDBParameterGroup_RefusesTheDefaultGroup(t *testing.T) {
	t.Parallel()
	h := newCreateHarness(t, testBaseDomain)

	_, err := h.svc.ModifyDBParameterGroup(t.Context(),
		modifyParameters(testDefaultPG, parameter("work_mem", "16384", "")), testAccountID)
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorInvalidParameterValue, awserrors.ValidErrorCodeFromError(err),
		"the code has to survive resolution or the client sees a 500")
	assert.Contains(t, err.Error(), "cannot be modified")
}

func TestModifyDBParameterGroup_RejectsAnUnknownGroup(t *testing.T) {
	t.Parallel()
	h := newCreateHarness(t, testBaseDomain)

	_, err := h.svc.ModifyDBParameterGroup(t.Context(),
		modifyParameters("absent", parameter("work_mem", "16384", "")), testAccountID)
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorDBParameterGroupNotFound, awserrors.ValidErrorCodeFromError(err),
		"the code has to survive resolution or the client sees a 500")
}

// A computed default reaches the customer as the literal it evaluated to,
// never as the formula that produced it.
func TestDescribeDBParameters_ReportsComputedDefaultsAsLiterals(t *testing.T) {
	t.Parallel()
	h := newCreateHarness(t, testBaseDomain)

	params := describedParameters(t, h, testDefaultPG)
	shared := params["shared_buffers"]
	require.NotNil(t, shared)

	// The class is named rather than derived from the class catalog, which would
	// make this assert the code agrees with itself.
	memoryMiB, err := classMemoryMiB("db.t3.micro")
	require.NoError(t, err)
	assert.Equal(t, sharedBuffersFor(memoryMiB), aws.StringValue(shared.ParameterValue))
	assert.Equal(t, ParameterSourceEngineDefault, aws.StringValue(shared.Source))
	assert.Equal(t, ApplyTypeStatic, aws.StringValue(shared.ApplyType))
	assert.Equal(t, ApplyMethodPendingReboot, aws.StringValue(shared.ApplyMethod))
	assert.True(t, aws.BoolValue(shared.IsModifiable))
	assert.NotEmpty(t, aws.StringValue(shared.AllowedValues))
}

// The canonical spelling the guest is handed is not what the API reports back:
// a Terraform plan comparing what it wrote against what it reads would otherwise
// show drift on every boolean, forever.
func TestDescribeDBParameters_ReportsTheCustomersBooleanSpelling(t *testing.T) {
	t.Parallel()
	h := newCreateHarness(t, testBaseDomain)
	_, err := h.svc.CreateDBParameterGroup(t.Context(), parameterGroupInput(testParameterGroup), testAccountID)
	require.NoError(t, err)
	_, err = h.svc.ModifyDBParameterGroup(t.Context(),
		modifyParameters(testParameterGroup, parameter("autovacuum", "off", "")), testAccountID)
	require.NoError(t, err)

	params := describedParameters(t, h, testParameterGroup)
	require.NotNil(t, params["autovacuum"])
	assert.Equal(t, "off", aws.StringValue(params["autovacuum"].ParameterValue))
	assert.Equal(t, ParameterSourceUser, aws.StringValue(params["autovacuum"].Source))

	resolved, err := enginePostgres.ResolveEffectiveParameters("db.t3.micro", map[string]string{"autovacuum": "off"})
	require.NoError(t, err)
	assert.Equal(t, "0", resolvedParameter(t, resolved, "autovacuum"),
		"the guest should be handed the canonical spelling rather than the customer's")
}

// Reported so a security-conscious plan can read the floor, refused so it cannot
// lower it. The refusal is the ordinary not-modifiable one, which names the
// parameter rather than claiming the engine has no such setting.
func TestDBParameterGroup_ReportsThePinnedTLSFloorAndRefusesToChangeIt(t *testing.T) {
	t.Parallel()
	h := newCreateHarness(t, testBaseDomain)
	_, err := h.svc.CreateDBParameterGroup(t.Context(), parameterGroupInput(testParameterGroup), testAccountID)
	require.NoError(t, err)

	floor := describedParameters(t, h, testParameterGroup)["ssl_min_protocol_version"]
	require.NotNil(t, floor)
	assert.Equal(t, "TLSv1.3", aws.StringValue(floor.ParameterValue))
	assert.Equal(t, ParameterSourceEngineDefault, aws.StringValue(floor.Source))
	assert.False(t, aws.BoolValue(floor.IsModifiable))

	_, err = h.svc.ModifyDBParameterGroup(t.Context(),
		modifyParameters(testParameterGroup, parameter("ssl_min_protocol_version", "TLSv1.2", "")), testAccountID)
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorInvalidParameterValue, awserrors.ValidErrorCodeFromError(err),
		"the code has to survive resolution or the client sees a 500")
	assert.Contains(t, err.Error(), "parameter ssl_min_protocol_version is not modifiable")
}

func TestDescribeDBParameters_DerivesApplyMethodForLegacyOverrides(t *testing.T) {
	t.Parallel()
	h := newCreateHarness(t, testBaseDomain)
	_, err := h.svc.CreateDBParameterGroup(t.Context(), parameterGroupInput(testParameterGroup), testAccountID)
	require.NoError(t, err)
	kv, err := h.svc.bucket(t.Context(), testAccountID)
	require.NoError(t, err)
	for _, rec := range []DBParameterRecord{
		{Name: "work_mem", Value: "16384"},
		{Name: "shared_buffers", Value: "65536"},
	} {
		require.NoError(t, putJSON(t.Context(), kv, DBParameterGroupParamKey(testParameterGroup, rec.Name), &rec))
	}

	params := describedParameters(t, h, testParameterGroup)
	assert.Equal(t, ApplyMethodImmediate, aws.StringValue(params["work_mem"].ApplyMethod))
	assert.Equal(t, ApplyMethodPendingReboot, aws.StringValue(params["shared_buffers"].ApplyMethod))
}

func TestDescribeDBParameters_FiltersOnSource(t *testing.T) {
	t.Parallel()
	h := newCreateHarness(t, testBaseDomain)
	_, err := h.svc.CreateDBParameterGroup(t.Context(), parameterGroupInput(testParameterGroup), testAccountID)
	require.NoError(t, err)
	_, err = h.svc.ModifyDBParameterGroup(t.Context(),
		modifyParameters(testParameterGroup, parameter("work_mem", "16384", "")), testAccountID)
	require.NoError(t, err)

	user, err := h.svc.DescribeDBParameters(t.Context(), &rds.DescribeDBParametersInput{
		DBParameterGroupName: aws.String(testParameterGroup),
		Source:               aws.String(ParameterSourceUser),
	}, testAccountID)
	require.NoError(t, err)
	require.Len(t, user.Parameters, 1)
	assert.Equal(t, "work_mem", aws.StringValue(user.Parameters[0].ParameterName))

	defaults, err := h.svc.DescribeDBParameters(t.Context(), &rds.DescribeDBParametersInput{
		DBParameterGroupName: aws.String(testParameterGroup),
		Source:               aws.String(ParameterSourceEngineDefault),
	}, testAccountID)
	require.NoError(t, err)
	assert.Len(t, defaults.Parameters, len(enginePostgres.CatalogParameterNames())-1)
}

func TestDeleteDBParameterGroup_RemovesTheGroupAndItsValues(t *testing.T) {
	t.Parallel()
	h := newCreateHarness(t, testBaseDomain)
	_, err := h.svc.CreateDBParameterGroup(t.Context(), parameterGroupInput(testParameterGroup), testAccountID)
	require.NoError(t, err)
	_, err = h.svc.ModifyDBParameterGroup(t.Context(),
		modifyParameters(testParameterGroup, parameter("work_mem", "16384", "")), testAccountID)
	require.NoError(t, err)

	_, err = h.svc.DeleteDBParameterGroup(t.Context(),
		&rds.DeleteDBParameterGroupInput{DBParameterGroupName: aws.String(testParameterGroup)}, testAccountID)
	require.NoError(t, err)

	kv, err := h.svc.bucket(t.Context(), testAccountID)
	require.NoError(t, err)
	// Orphaned values would be silently inherited by a later group of the same
	// name, which is a configuration nobody wrote.
	overrides, err := ListDBParameterOverrides(t.Context(), kv, testParameterGroup)
	require.NoError(t, err)
	assert.Empty(t, overrides)

	_, err = h.svc.DescribeDBParameterGroups(t.Context(),
		&rds.DescribeDBParameterGroupsInput{DBParameterGroupName: aws.String(testParameterGroup)}, testAccountID)
	assert.Equal(t, awserrors.ErrorDBParameterGroupNotFound, awserrors.ValidErrorCodeFromError(err),
		"the code has to survive resolution or the client sees a 500")
}

func TestDeleteDBParameterGroup_RefusesTheDefaultGroup(t *testing.T) {
	t.Parallel()
	h := newCreateHarness(t, testBaseDomain)

	_, err := h.svc.DeleteDBParameterGroup(t.Context(),
		&rds.DeleteDBParameterGroupInput{DBParameterGroupName: aws.String(testDefaultPG)}, testAccountID)
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorDBParameterGroupInvalidState, awserrors.ValidErrorCodeFromError(err),
		"the code has to survive resolution or the client sees a 500")
}

// Including a group only a deleting instance still names, so a destroy that
// races the teardown fails cleanly rather than stranding a live engine.
func TestDeleteDBParameterGroup_RefusesWhileAnInstanceReferencesIt(t *testing.T) {
	for _, status := range []Status{StatusAvailable, StatusDeleting} {
		t.Run(string(status), func(t *testing.T) {
			h := newCreateHarness(t, testBaseDomain)
			_, err := h.svc.CreateDBParameterGroup(t.Context(), parameterGroupInput(testParameterGroup), testAccountID)
			require.NoError(t, err)

			input := validCreateInput()
			input.DBParameterGroupName = aws.String(testParameterGroup)
			_, err = h.svc.CreateDBInstance(t.Context(), input, testAccountID)
			require.NoError(t, err)

			kv, err := h.svc.bucket(t.Context(), testAccountID)
			require.NoError(t, err)
			require.NoError(t, h.svc.updateInstance(t.Context(), kv, testDBInstanceID, func(rec *DBInstanceRecord) {
				rec.Status = status
			}))

			_, err = h.svc.DeleteDBParameterGroup(t.Context(),
				&rds.DeleteDBParameterGroupInput{DBParameterGroupName: aws.String(testParameterGroup)}, testAccountID)
			require.Error(t, err)
			assert.Equal(t, awserrors.ErrorDBParameterGroupInvalidState, awserrors.ValidErrorCodeFromError(err),
				"the code has to survive resolution or the client sees a 500")
			assert.Contains(t, err.Error(), testDBInstanceID)
		})
	}
}

// The group is the thing bootstrap consumes, so a create has to resolve it into
// the literals the agent installs rather than leaving the set empty.
func TestCreateDBInstance_ResolvesTheNamedGroupIntoTheBootstrapSet(t *testing.T) {
	t.Parallel()
	h := newCreateHarness(t, testBaseDomain)
	_, err := h.svc.CreateDBParameterGroup(t.Context(), parameterGroupInput(testParameterGroup), testAccountID)
	require.NoError(t, err)
	_, err = h.svc.ModifyDBParameterGroup(t.Context(),
		modifyParameters(testParameterGroup, parameter("work_mem", "16384", "")), testAccountID)
	require.NoError(t, err)

	input := validCreateInput()
	input.DBParameterGroupName = aws.String(testParameterGroup)
	_, err = h.svc.CreateDBInstance(t.Context(), input, testAccountID)
	require.NoError(t, err)

	rec := h.record(t, testDBInstanceID)
	assert.Equal(t, testParameterGroup, rec.DBParameterGroupName)
	require.Len(t, rec.Bootstrap.ResolvedParameters, len(enginePostgres.CatalogParameterNames()))

	values := map[string]string{}
	for _, param := range rec.Bootstrap.ResolvedParameters {
		values[param.Name] = param.Value
	}
	assert.Equal(t, "16384", values["work_mem"])
	// Evaluated against the instance's own class, which is the whole point of a
	// computed default: a literal tuned elsewhere would be wrong here.
	memoryMiB, err := classMemoryMiB("db.t3.medium")
	require.NoError(t, err)
	assert.Equal(t, sharedBuffersFor(memoryMiB), values["shared_buffers"])
}

// An unnamed group takes the implicit default, which resolves to the catalog
// alone rather than to nothing.
func TestCreateDBInstance_ResolvesTheDefaultGroupWhenNoneIsNamed(t *testing.T) {
	t.Parallel()
	h := newCreateHarness(t, testBaseDomain)

	_, err := h.svc.CreateDBInstance(t.Context(), validCreateInput(), testAccountID)
	require.NoError(t, err)

	rec := h.record(t, testDBInstanceID)
	assert.Equal(t, testDefaultPG, rec.DBParameterGroupName)
	assert.Len(t, rec.Bootstrap.ResolvedParameters, len(enginePostgres.CatalogParameterNames()))
}

func TestCreateDBInstance_RejectsAnUnknownParameterGroup(t *testing.T) {
	t.Parallel()
	h := newCreateHarness(t, testBaseDomain)

	input := validCreateInput()
	input.DBParameterGroupName = aws.String("absent")
	_, err := h.svc.CreateDBInstance(t.Context(), input, testAccountID)
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorDBParameterGroupNotFound, awserrors.ValidErrorCodeFromError(err),
		"the code has to survive resolution or the client sees a 500")
	assert.False(t, h.recordExists(t, testDBInstanceID), "a rejected create must reserve nothing")
}

// Both groups are taggable through the one ARN-addressed mutate path, so the
// registry entries are what make an ARN a resource rather than a rejection.
func TestTagActions_ReachBothGroupTypes(t *testing.T) {
	t.Parallel()
	h := newCreateHarness(t, testBaseDomain)
	_, err := h.svc.CreateDBSubnetGroup(t.Context(), subnetGroupInput(testSubnetGroup, "subnet-alpha"), testAccountID)
	require.NoError(t, err)
	_, err = h.svc.CreateDBParameterGroup(t.Context(), parameterGroupInput(testParameterGroup), testAccountID)
	require.NoError(t, err)

	for _, arn := range []string{
		FormatARN(ResourceKindDBSubnetGroup, testRegion, testAccountID, testSubnetGroup),
		FormatARN(ResourceKindDBParameterGroup, testRegion, testAccountID, testParameterGroup),
	} {
		_, err := h.svc.AddTagsToResource(t.Context(), &rds.AddTagsToResourceInput{
			ResourceName: aws.String(arn),
			Tags:         []*rds.Tag{{Key: aws.String("env"), Value: aws.String("prod")}},
		}, testAccountID)
		require.NoError(t, err, "%s", arn)

		listed, err := h.svc.ListTagsForResource(t.Context(),
			&rds.ListTagsForResourceInput{ResourceName: aws.String(arn)}, testAccountID)
		require.NoError(t, err)
		require.Len(t, listed.TagList, 1)
		assert.Equal(t, "prod", aws.StringValue(listed.TagList[0].Value))
	}
}
