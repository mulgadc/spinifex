package handlers_ec2_instance

import (
	"context"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	"github.com/mulgadc/spinifex/spinifex/config"
	"github.com/mulgadc/spinifex/spinifex/utils"
	"github.com/mulgadc/spinifex/spinifex/vm"
	"github.com/mulgadc/viperblock/viperblock"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The shared validator passes the secure/default values (and AWS "leave
// unchanged" empties) as no-ops, accepts either http-tokens state, rejects any
// unmodelled feature with UnsupportedOperation, and bounds the hop limit to 1-64.
func TestValidateMetadataOptions(t *testing.T) {
	cases := map[string]struct {
		httpTokens, httpEndpoint, ipv6, tags string
		hopLimit                             *int64
		wantCode                             string
	}{
		"all omitted":            {},
		"secure values":          {ec2.HttpTokensStateRequired, ec2.InstanceMetadataEndpointStateEnabled, ec2.InstanceMetadataProtocolStateDisabled, ec2.InstanceMetadataTagsStateDisabled, aws.Int64(1), ""},
		"hop limit lower bound":  {hopLimit: aws.Int64(1)},
		"hop limit upper bound":  {hopLimit: aws.Int64(64)},
		"tokens optional":        {httpTokens: ec2.HttpTokensStateOptional},
		"tokens unrecognised":    {httpTokens: "sometimes", wantCode: awserrors.ErrorUnsupportedOperation},
		"endpoint disabled":      {httpEndpoint: ec2.InstanceMetadataEndpointStateDisabled, wantCode: awserrors.ErrorUnsupportedOperation},
		"ipv6 enabled":           {ipv6: ec2.InstanceMetadataProtocolStateEnabled, wantCode: awserrors.ErrorUnsupportedOperation},
		"tags enabled":           {tags: ec2.InstanceMetadataTagsStateEnabled, wantCode: awserrors.ErrorUnsupportedOperation},
		"hop limit zero":         {hopLimit: aws.Int64(0), wantCode: awserrors.ErrorInvalidParameterValue},
		"hop limit over maximum": {hopLimit: aws.Int64(65), wantCode: awserrors.ErrorInvalidParameterValue},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			err := validateMetadataOptions(tc.httpTokens, tc.httpEndpoint, tc.ipv6, tc.tags, tc.hopLimit)
			if tc.wantCode == "" {
				require.NoError(t, err)
				return
			}
			require.Error(t, err)
			assert.True(t, awserrors.IsErrorCode(err, tc.wantCode), "want %s, got %v", tc.wantCode, err)
		})
	}
}

// Enabling IMDSv1 on an existing instance is applied and echoed back; IMDSv1-only
// guest agents can be switched on after launch without a relaunch.
func TestModifyInstanceMetadataOptions_EnableV1(t *testing.T) {
	owner := utils.GlobalAccountID
	id := "i-imdsv1"
	v := &vm.VM{
		ID: id, AccountID: owner, Status: vm.StateRunning,
		Instance: &ec2.Instance{InstanceId: aws.String(id), MetadataOptions: buildMetadataOptions(nil, "")},
	}
	svc := &InstanceServiceImpl{vmMgr: mgrWith(map[string]*vm.VM{id: v})}

	out, err := svc.ModifyInstanceMetadataOptions(context.Background(), &ec2.ModifyInstanceMetadataOptionsInput{
		InstanceId: aws.String(id),
		HttpTokens: aws.String(ec2.HttpTokensStateOptional),
	}, owner)
	require.NoError(t, err)
	require.NotNil(t, out.InstanceMetadataOptions)
	assert.Equal(t, ec2.HttpTokensStateOptional, aws.StringValue(out.InstanceMetadataOptions.HttpTokens))
	assert.Equal(t, ec2.HttpTokensStateOptional, aws.StringValue(v.Instance.MetadataOptions.HttpTokens))
	// The hop limit was not in the request, so it must not move.
	assert.Equal(t, int64(1), aws.Int64Value(v.Instance.MetadataOptions.HttpPutResponseHopLimit))
}

// An unrecognised http-tokens state is still refused, and the instance is left
// untouched — no partial application of a rejected request.
func TestModifyInstanceMetadataOptions_RejectUnknownTokensState(t *testing.T) {
	owner := utils.GlobalAccountID
	id := "i-imdsbad"
	v := &vm.VM{
		ID: id, AccountID: owner, Status: vm.StateRunning,
		Instance: &ec2.Instance{InstanceId: aws.String(id), MetadataOptions: buildMetadataOptions(nil, "")},
	}
	svc := &InstanceServiceImpl{vmMgr: mgrWith(map[string]*vm.VM{id: v})}

	_, err := svc.ModifyInstanceMetadataOptions(context.Background(), &ec2.ModifyInstanceMetadataOptionsInput{
		InstanceId: aws.String(id),
		HttpTokens: aws.String("sometimes"),
	}, owner)
	require.Error(t, err)
	assert.True(t, awserrors.IsErrorCode(err, awserrors.ErrorUnsupportedOperation), "got %v", err)
	assert.Equal(t, ec2.HttpTokensStateRequired, aws.StringValue(v.Instance.MetadataOptions.HttpTokens))
}

// A hop-limit change on a running instance persists and is echoed back — AWS
// allows this in any state, so it must not return IncorrectInstanceState. The
// no-op http-tokens=required is accepted alongside it.
func TestModifyInstanceMetadataOptions_HopLimitRunning(t *testing.T) {
	owner := utils.GlobalAccountID
	id := "i-hop-run"
	v := &vm.VM{
		ID: id, AccountID: owner, Status: vm.StateRunning,
		Instance: &ec2.Instance{InstanceId: aws.String(id), MetadataOptions: buildMetadataOptions(nil, "")},
	}
	svc := &InstanceServiceImpl{vmMgr: mgrWith(map[string]*vm.VM{id: v})}

	out, err := svc.ModifyInstanceMetadataOptions(context.Background(), &ec2.ModifyInstanceMetadataOptionsInput{
		InstanceId:              aws.String(id),
		HttpTokens:              aws.String(ec2.HttpTokensStateRequired),
		HttpPutResponseHopLimit: aws.Int64(2),
	}, owner)
	require.NoError(t, err)
	require.NotNil(t, out.InstanceMetadataOptions)
	assert.Equal(t, int64(2), aws.Int64Value(out.InstanceMetadataOptions.HttpPutResponseHopLimit))
	assert.Equal(t, ec2.HttpTokensStateRequired, aws.StringValue(out.InstanceMetadataOptions.HttpTokens))
	assert.Equal(t, int64(2), aws.Int64Value(v.Instance.MetadataOptions.HttpPutResponseHopLimit))
}

// The stopped-store fallback persists the hop-limit change too.
func TestModifyInstanceMetadataOptions_HopLimitStopped(t *testing.T) {
	owner := utils.GlobalAccountID
	id := "i-hop-stop"
	stored := &vm.VM{
		ID: id, AccountID: owner, Status: vm.StateStopped,
		Instance: &ec2.Instance{InstanceId: aws.String(id), MetadataOptions: buildMetadataOptions(nil, "")},
	}
	store := &fakeStoppedStore{loadByID: map[string]*vm.VM{id: stored}}
	svc := &InstanceServiceImpl{vmMgr: mgrWith(map[string]*vm.VM{}), stoppedStore: store}

	out, err := svc.ModifyInstanceMetadataOptions(context.Background(), &ec2.ModifyInstanceMetadataOptionsInput{
		InstanceId:              aws.String(id),
		HttpPutResponseHopLimit: aws.Int64(3),
	}, owner)
	require.NoError(t, err)
	assert.Equal(t, int64(3), aws.Int64Value(out.InstanceMetadataOptions.HttpPutResponseHopLimit))
	require.Contains(t, store.wroteStopped, id)
	assert.Equal(t, int64(3), aws.Int64Value(store.wroteStopped[id].Instance.MetadataOptions.HttpPutResponseHopLimit))
}

// An unknown instance is reported absent, not a server error.
func TestModifyInstanceMetadataOptions_NotFound(t *testing.T) {
	svc := &InstanceServiceImpl{
		vmMgr:        mgrWith(map[string]*vm.VM{}),
		stoppedStore: &fakeStoppedStore{loadByID: map[string]*vm.VM{}},
	}
	_, err := svc.ModifyInstanceMetadataOptions(context.Background(), &ec2.ModifyInstanceMetadataOptionsInput{
		InstanceId:              aws.String("i-ghost"),
		HttpPutResponseHopLimit: aws.Int64(2),
	}, utils.GlobalAccountID)
	require.Error(t, err)
	assert.True(t, awserrors.IsErrorCode(err, awserrors.ErrorInvalidInstanceIDNotFound), "got %v", err)
}

// A nil Instance (data-integrity case the sibling ModifyInstanceAttribute also
// guards) returns ServerInternal rather than panicking the daemon — on both the
// running and stopped paths.
func TestModifyInstanceMetadataOptions_NilInstance(t *testing.T) {
	owner := utils.GlobalAccountID

	t.Run("running", func(t *testing.T) {
		id := "i-nil-run"
		v := &vm.VM{ID: id, AccountID: owner, Status: vm.StateRunning, Instance: nil}
		svc := &InstanceServiceImpl{vmMgr: mgrWith(map[string]*vm.VM{id: v})}
		_, err := svc.ModifyInstanceMetadataOptions(context.Background(), &ec2.ModifyInstanceMetadataOptionsInput{
			InstanceId: aws.String(id), HttpPutResponseHopLimit: aws.Int64(2),
		}, owner)
		require.Error(t, err)
		assert.True(t, awserrors.IsErrorCode(err, awserrors.ErrorServerInternal), "got %v", err)
	})

	t.Run("stopped", func(t *testing.T) {
		id := "i-nil-stop"
		stored := &vm.VM{ID: id, AccountID: owner, Status: vm.StateStopped, Instance: nil}
		store := &fakeStoppedStore{loadByID: map[string]*vm.VM{id: stored}}
		svc := &InstanceServiceImpl{vmMgr: mgrWith(map[string]*vm.VM{}), stoppedStore: store}
		_, err := svc.ModifyInstanceMetadataOptions(context.Background(), &ec2.ModifyInstanceMetadataOptionsInput{
			InstanceId: aws.String(id), HttpPutResponseHopLimit: aws.Int64(2),
		}, owner)
		require.Error(t, err)
		assert.True(t, awserrors.IsErrorCode(err, awserrors.ErrorServerInternal), "got %v", err)
	})
}

// A legacy instance launched before the constant block (nil MetadataOptions) is
// stamped with the full IMDSv2-only block on the first modify, not just the hop.
func TestModifyInstanceMetadataOptions_LegacyNilBlockStamped(t *testing.T) {
	owner := utils.GlobalAccountID
	id := "i-legacy"
	v := &vm.VM{
		ID: id, AccountID: owner, Status: vm.StateRunning,
		Instance: &ec2.Instance{InstanceId: aws.String(id)},
	}
	svc := &InstanceServiceImpl{vmMgr: mgrWith(map[string]*vm.VM{id: v})}

	out, err := svc.ModifyInstanceMetadataOptions(context.Background(), &ec2.ModifyInstanceMetadataOptionsInput{
		InstanceId: aws.String(id), HttpPutResponseHopLimit: aws.Int64(2),
	}, owner)
	require.NoError(t, err)
	require.NotNil(t, out.InstanceMetadataOptions)
	assert.Equal(t, ec2.HttpTokensStateRequired, aws.StringValue(out.InstanceMetadataOptions.HttpTokens))
	assert.Equal(t, int64(2), aws.Int64Value(out.InstanceMetadataOptions.HttpPutResponseHopLimit))
	require.NotNil(t, v.Instance.MetadataOptions, "the legacy instance must be stamped in place")
}

// The constant block reports the IMDSv2-only posture: required tokens, endpoint
// enabled, IPv6/tags metadata disabled, state applied. Only the hop limit moves.
func TestBuildMetadataOptions_ConstantBlock(t *testing.T) {
	opts := buildMetadataOptions(nil, "")
	require.NotNil(t, opts)
	assert.Equal(t, ec2.InstanceMetadataOptionsStateApplied, aws.StringValue(opts.State))
	assert.Equal(t, ec2.HttpTokensStateRequired, aws.StringValue(opts.HttpTokens))
	assert.Equal(t, ec2.InstanceMetadataEndpointStateEnabled, aws.StringValue(opts.HttpEndpoint))
	assert.Equal(t, ec2.InstanceMetadataProtocolStateDisabled, aws.StringValue(opts.HttpProtocolIpv6))
	assert.Equal(t, ec2.InstanceMetadataTagsStateDisabled, aws.StringValue(opts.InstanceMetadataTags))
	assert.Equal(t, int64(1), aws.Int64Value(opts.HttpPutResponseHopLimit), "nil hop limit defaults to 1")
}

func TestBuildMetadataOptions_HopLimit(t *testing.T) {
	cases := map[string]struct {
		in   *int64
		want int64
	}{
		"in-range 2":   {aws.Int64(2), 2},
		"upper bound":  {aws.Int64(64), 64},
		"lower bound":  {aws.Int64(1), 1},
		"zero clamps":  {aws.Int64(0), 1},
		"over clamps":  {aws.Int64(65), 1},
		"nil defaults": {nil, 1},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			assert.Equal(t, tc.want, aws.Int64Value(buildMetadataOptions(tc.in, "").HttpPutResponseHopLimit))
		})
	}
}

// Every instance launched after this change carries the block so
// DescribeInstances surfaces the posture without a projection change. A launch
// that says nothing about http-tokens must still land on "required" — this is
// the guard against the IMDSv1 opt-in becoming the default by accident.
func TestRunInstance_MetadataOptionsSeeded(t *testing.T) {
	svc := &InstanceServiceImpl{
		instanceTypes: map[string]*ec2.InstanceTypeInfo{"t3.micro": {InstanceType: aws.String("t3.micro")}},
	}

	_, ec2Instance, err := svc.RunInstance(&ec2.RunInstancesInput{
		ImageId:      aws.String("ami-0abcdef1234567890"),
		InstanceType: aws.String("t3.micro"),
	})
	require.NoError(t, err)
	require.NotNil(t, ec2Instance.MetadataOptions, "launch must stamp the metadata-options block")
	assert.Equal(t, ec2.HttpTokensStateRequired, aws.StringValue(ec2Instance.MetadataOptions.HttpTokens))
	assert.Equal(t, int64(1), aws.Int64Value(ec2Instance.MetadataOptions.HttpPutResponseHopLimit))
}

// A launch asking for IMDSv1 passes validation and the opt-in reaches the
// instance, which is what lets an IMDSv1-only guest agent bootstrap.
func TestRunInstance_MetadataOptionsTokensOptional(t *testing.T) {
	svc := &InstanceServiceImpl{
		instanceTypes: map[string]*ec2.InstanceTypeInfo{"t3.micro": {InstanceType: aws.String("t3.micro")}},
	}

	_, ec2Instance, err := svc.RunInstance(&ec2.RunInstancesInput{
		ImageId:         aws.String("ami-0abcdef1234567890"),
		InstanceType:    aws.String("t3.micro"),
		MetadataOptions: &ec2.InstanceMetadataOptionsRequest{HttpTokens: aws.String(ec2.HttpTokensStateOptional)},
	})
	require.NoError(t, err)
	require.NotNil(t, ec2Instance.MetadataOptions)
	assert.Equal(t, ec2.HttpTokensStateOptional, aws.StringValue(ec2Instance.MetadataOptions.HttpTokens))
}

// An unmodelled metadata option is still rejected before any capacity is
// allocated. Validation precedes the instance-type lookup, so a bare service
// suffices.
func TestPrepareRunInstances_RejectUnsupportedMetadataOptions(t *testing.T) {
	svc := &InstanceServiceImpl{}
	_, _, _, err := svc.PrepareRunInstances(context.Background(), &ec2.RunInstancesInput{
		ImageId:      aws.String("ami-0abcdef1234567890"),
		InstanceType: aws.String("t3.micro"),
		MinCount:     aws.Int64(1),
		MaxCount:     aws.Int64(1),
		MetadataOptions: &ec2.InstanceMetadataOptionsRequest{
			HttpEndpoint: aws.String(ec2.InstanceMetadataEndpointStateDisabled),
		},
	}, utils.GlobalAccountID, "")
	require.Error(t, err)
	assert.True(t, awserrors.IsErrorCode(err, awserrors.ErrorUnsupportedOperation), "got %v", err)
}

// A requested hop limit is reflected on the instance; the rest stay invariant.
func TestRunInstance_MetadataOptionsHopLimitFromRequest(t *testing.T) {
	svc := &InstanceServiceImpl{
		instanceTypes: map[string]*ec2.InstanceTypeInfo{"t3.micro": {InstanceType: aws.String("t3.micro")}},
	}

	_, ec2Instance, err := svc.RunInstance(&ec2.RunInstancesInput{
		ImageId:         aws.String("ami-0abcdef1234567890"),
		InstanceType:    aws.String("t3.micro"),
		MetadataOptions: &ec2.InstanceMetadataOptionsRequest{HttpPutResponseHopLimit: aws.Int64(2)},
	})
	require.NoError(t, err)
	require.NotNil(t, ec2Instance.MetadataOptions)
	assert.Equal(t, int64(2), aws.Int64Value(ec2Instance.MetadataOptions.HttpPutResponseHopLimit))
	assert.Equal(t, ec2.HttpTokensStateRequired, aws.StringValue(ec2Instance.MetadataOptions.HttpTokens))
}

// Windows is the one platform whose guest agent cannot speak IMDSv2, so it is
// the one platform whose launch default is relaxed. Anything else — including
// an image whose platform never resolved — stays on "required".
func TestDefaultHTTPTokensForPlatform(t *testing.T) {
	cases := map[string]struct {
		in   *string
		want string
	}{
		"windows":     {aws.String(utils.PlatformWindows), ec2.HttpTokensStateOptional},
		"nil (linux)": {nil, ec2.HttpTokensStateRequired},
		"empty":       {aws.String(""), ec2.HttpTokensStateRequired},
		"unexpected":  {aws.String("Windows"), ec2.HttpTokensStateRequired},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			assert.Equal(t, tc.want, defaultHTTPTokensForPlatform(tc.in))
		})
	}
}

// The platform default only fills a gap: it moves a Windows instance the launch
// said nothing about, and never overrides a value the caller named — not even
// a Windows launch that deliberately asks for the strict posture.
func TestApplyPlatformTokenDefault(t *testing.T) {
	windows := aws.String(utils.PlatformWindows)

	cases := map[string]struct {
		platform  *string
		requested string
		want      string
	}{
		"windows, unspecified":         {windows, "", ec2.HttpTokensStateOptional},
		"linux, unspecified":           {nil, "", ec2.HttpTokensStateRequired},
		"windows, explicitly required": {windows, ec2.HttpTokensStateRequired, ec2.HttpTokensStateRequired},
		"linux, explicitly optional":   {nil, ec2.HttpTokensStateOptional, ec2.HttpTokensStateOptional},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			instance := &ec2.Instance{MetadataOptions: buildMetadataOptions(nil, tc.requested)}
			applyPlatformTokenDefault(instance, tc.requested, tc.platform)
			assert.Equal(t, tc.want, aws.StringValue(instance.MetadataOptions.HttpTokens))
		})
	}
}

// A legacy instance carrying no metadata-options block is left alone rather
// than panicking; stamping one is applyMetadataOptions' job.
func TestApplyPlatformTokenDefaultNilBlock(t *testing.T) {
	instance := &ec2.Instance{}
	applyPlatformTokenDefault(instance, "", aws.String(utils.PlatformWindows))
	assert.Nil(t, instance.MetadataOptions)
}

// End to end through the launch path: the default follows the resolved image's
// PlatformDetails, and a caller who names http-tokens still gets what they
// asked for on either platform.
func TestPrepareRunInstances_PlatformTokenDefault(t *testing.T) {
	tests := []struct {
		name            string
		platformDetails string
		requested       string
		want            string
	}{
		{"windows ami, unspecified", "Windows", "", ec2.HttpTokensStateOptional},
		{"windows byol ami, unspecified", "Windows BYOL", "", ec2.HttpTokensStateOptional},
		{"linux ami, unspecified", "Linux/UNIX", "", ec2.HttpTokensStateRequired},
		{"windows ami, explicitly required", "Windows", ec2.HttpTokensStateRequired, ec2.HttpTokensStateRequired},
		{"linux ami, explicitly optional", "Linux/UNIX", ec2.HttpTokensStateOptional, ec2.HttpTokensStateOptional},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			types, _ := defaultPrepareInstanceTypes()
			svc := &InstanceServiceImpl{
				config:        &config.Config{},
				instanceTypes: types,
				amiLoader: &fakeAMILoader{byID: map[string]viperblock.AMIMetadata{
					"ami-1": {ImageOwnerAlias: "acc", PlatformDetails: tc.platformDetails},
				}},
				resourceMgr: &fakeResourceCapacityProvider{
					instanceTypes: types,
					canAllocFn:    func(_ *ec2.InstanceTypeInfo, count int) int { return count },
				},
			}
			input := &ec2.RunInstancesInput{
				InstanceType: aws.String("t3.micro"),
				ImageId:      aws.String("ami-1"),
				MinCount:     aws.Int64(1),
				MaxCount:     aws.Int64(1),
			}
			if tc.requested != "" {
				input.MetadataOptions = &ec2.InstanceMetadataOptionsRequest{HttpTokens: aws.String(tc.requested)}
			}
			reservation, _, _, err := svc.PrepareRunInstances(context.Background(), input, "acc", "")
			require.NoError(t, err)
			require.Len(t, reservation.Instances, 1)
			require.NotNil(t, reservation.Instances[0].MetadataOptions)
			assert.Equal(t, tc.want, aws.StringValue(reservation.Instances[0].MetadataOptions.HttpTokens))
		})
	}
}
