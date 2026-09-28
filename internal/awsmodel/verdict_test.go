package awsmodel_test

import (
	"net/http"
	"testing"

	"github.com/mulgadc/spinifex/internal/awsmodel"
	"github.com/stretchr/testify/require"
)

var (
	accepted       = awsmodel.RequestResult{Status: 200}
	invalid        = awsmodel.RequestResult{Status: 400, Code: "ValidationError"}
	missing        = awsmodel.RequestResult{Status: 400, Code: "MissingParameter"}
	notFound       = awsmodel.RequestResult{Status: 404, Code: "NoSuchEntity"}
	createRolePlan = awsmodel.RequestPlan{DeclaredErrors: []string{"InvalidInput", "LimitExceeded"}}
)

func TestRequestJudgeAcceptance(t *testing.T) {
	tests := []struct {
		name   string
		result awsmodel.RequestResult
		want   awsmodel.Verdict
	}{
		{"accepted valid request", accepted, awsmodel.VerdictPass},
		{"valid request refused as invalid", invalid, awsmodel.VerdictFinding},
		{"valid request refused as malformed", awsmodel.RequestResult{Status: 400, Code: "InvalidInstanceID.Malformed"}, awsmodel.VerdictFinding},
		{"valid request missing a conditionally required member", missing, awsmodel.VerdictInconclusive},
		{"valid request names a missing resource", notFound, awsmodel.VerdictInconclusive},
		{"valid request times out", awsmodel.RequestResult{Code: "Timeout"}, awsmodel.VerdictInconclusive},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			require.Equal(t, test.want, createRolePlan.NewJudge().Judge(awsmodel.RequestCase{}, test.result))
		})
	}
}

func TestRequestJudgeRejection(t *testing.T) {
	rejection := awsmodel.RequestCase{Constraint: awsmodel.ConstraintLength, Path: "$.RoleName"}
	tests := []struct {
		name   string
		plan   awsmodel.RequestPlan
		result awsmodel.RequestResult
		want   awsmodel.Verdict
	}{
		{"invalid request accepted", createRolePlan, accepted, awsmodel.VerdictFinding},
		{"refused with a common validation error", createRolePlan, invalid, awsmodel.VerdictPass},
		{"refused for a missing member", createRolePlan, missing, awsmodel.VerdictPass},
		{"refused with a declared error", createRolePlan, awsmodel.RequestResult{Status: 400, Code: "InvalidInput"}, awsmodel.VerdictPass},
		{"refused with an undeclared error", createRolePlan, awsmodel.RequestResult{Status: 400, Code: "InvalidParameterValueException"}, awsmodel.VerdictUndeclaredError},
		{"operation declares no errors", awsmodel.RequestPlan{}, awsmodel.RequestResult{Status: 400, Code: "InvalidParameterValueException"}, awsmodel.VerdictPass},
		{"refused for another reason", createRolePlan, notFound, awsmodel.VerdictInconclusive},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			judge := test.plan.NewJudge()
			require.Equal(t, awsmodel.VerdictPass, judge.Judge(awsmodel.RequestCase{}, accepted))
			require.Equal(t, test.want, judge.Judge(rejection, test.result))
		})
	}
}

func TestRequestJudgeDiscountsRefusalsOfRefusedAcceptance(t *testing.T) {
	judge := createRolePlan.NewJudge()
	require.Equal(t, awsmodel.VerdictPass, judge.Judge(awsmodel.RequestCase{}, accepted))
	require.Equal(t, awsmodel.VerdictFinding, judge.Judge(awsmodel.RequestCase{Member: "Tags"}, invalid))
	require.Equal(t, awsmodel.VerdictPass, judge.Judge(awsmodel.RequestCase{Member: "Path"}, accepted))
	// Tags alone was refused, so a refusal of its rejection proves nothing.
	require.Equal(t, awsmodel.VerdictInconclusive, judge.Judge(awsmodel.RequestCase{Member: "Tags", Constraint: awsmodel.ConstraintLength}, invalid))
	require.Equal(t, awsmodel.VerdictFinding, judge.Judge(awsmodel.RequestCase{Member: "Tags", Constraint: awsmodel.ConstraintLength}, accepted))
	require.Equal(t, awsmodel.VerdictPass, judge.Judge(awsmodel.RequestCase{Member: "Path", Constraint: awsmodel.ConstraintPattern}, invalid))

	// A refused base leaves no refusal attributable to any single member, but
	// a request breaking a constraint that succeeds is still a finding.
	judge = createRolePlan.NewJudge()
	require.Equal(t, awsmodel.VerdictInconclusive, judge.Judge(awsmodel.RequestCase{}, missing))
	require.Equal(t, awsmodel.VerdictInconclusive, judge.Judge(awsmodel.RequestCase{Constraint: awsmodel.ConstraintRequired, Path: "$.RoleName"}, invalid))
	require.Equal(t, awsmodel.VerdictFinding, judge.Judge(awsmodel.RequestCase{Constraint: awsmodel.ConstraintRequired, Path: "$.RoleName"}, accepted))
	require.Equal(t, awsmodel.VerdictInconclusive, judge.Judge(awsmodel.RequestCase{Member: "Tags"}, accepted))
	require.Equal(t, awsmodel.VerdictFinding, judge.Judge(awsmodel.RequestCase{Member: "Tags", Constraint: awsmodel.ConstraintLength}, accepted))
}

func TestRequestJudgeDiscountsRefusalsOfMembersWithoutAcceptance(t *testing.T) {
	judge := createRolePlan.NewJudge()
	require.Equal(t, awsmodel.VerdictPass, judge.Judge(awsmodel.RequestCase{}, accepted))
	// A pagination token gets no acceptance case, so its refusal may be of
	// the token rather than the broken constraint.
	marker := awsmodel.RequestCase{Member: "Marker", Constraint: awsmodel.ConstraintLength, Path: "$.Marker"}
	require.Equal(t, awsmodel.VerdictInconclusive, judge.Judge(marker, invalid))
	require.Equal(t, awsmodel.VerdictFinding, judge.Judge(marker, accepted))
}

func TestDecodeRequestResult(t *testing.T) {
	tests := []struct {
		name    string
		service awsmodel.Service
		header  http.Header
		body    string
		code    string
		message string
	}{
		{"awsQuery", awsmodel.IAM, nil,
			`<ErrorResponse><Error><Type>Sender</Type><Code>ValidationError</Code><Message>bad name</Message></Error></ErrorResponse>`,
			"ValidationError", "bad name"},
		{"ec2", awsmodel.EC2, nil,
			`<Response><Errors><Error><Code>InvalidParameterValue</Code><Message>bad id</Message></Error></Errors><RequestID>r</RequestID></Response>`,
			"InvalidParameterValue", "bad id"},
		{"json", awsmodel.ECS, nil,
			`{"__type":"com.amazonaws.ecs#InvalidParameterException","message":"bad cluster"}`,
			"InvalidParameterException", "bad cluster"},
		{"rest-json header", awsmodel.EKS, http.Header{"X-Amzn-Errortype": {"InvalidParameterException:http://internal.amazon.com/"}},
			`{"message":"bad name"}`,
			"InvalidParameterException", "bad name"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			result := awsmodel.DecodeRequestResult(test.service, 400, test.header, []byte(test.body))
			require.Equal(t, awsmodel.RequestResult{Status: 400, Code: test.code, Message: test.message}, result)
		})
	}
	require.Equal(t, awsmodel.RequestResult{Status: 200}, awsmodel.DecodeRequestResult(awsmodel.IAM, 200, nil, []byte("<ok/>")))
}
