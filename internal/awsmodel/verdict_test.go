package awsmodel_test

import (
	"net/http"
	"testing"

	"github.com/mulgadc/spinifex/internal/awsmodel"
	"github.com/stretchr/testify/require"
)

func TestRequestCaseJudge(t *testing.T) {
	acceptance := awsmodel.RequestCase{Operation: "CreateRole"}
	rejection := awsmodel.RequestCase{Operation: "CreateRole", Constraint: awsmodel.ConstraintLength, Path: "$.RoleName"}
	tests := []struct {
		name    string
		request awsmodel.RequestCase
		result  awsmodel.RequestResult
		want    awsmodel.Verdict
	}{
		{"accepted valid request", acceptance, awsmodel.RequestResult{Status: 200}, awsmodel.VerdictPass},
		{"valid request refused as invalid", acceptance, awsmodel.RequestResult{Status: 400, Code: "ValidationError"}, awsmodel.VerdictFinding},
		{"valid request refused as malformed", acceptance, awsmodel.RequestResult{Status: 400, Code: "InvalidInstanceID.Malformed"}, awsmodel.VerdictFinding},
		{"valid request missing a conditionally required member", acceptance, awsmodel.RequestResult{Status: 400, Code: "MissingParameter"}, awsmodel.VerdictInconclusive},
		{"valid request names a missing resource", acceptance, awsmodel.RequestResult{Status: 404, Code: "NoSuchEntity"}, awsmodel.VerdictInconclusive},
		{"valid request times out", acceptance, awsmodel.RequestResult{Code: "Timeout"}, awsmodel.VerdictInconclusive},
		{"invalid request accepted", rejection, awsmodel.RequestResult{Status: 200}, awsmodel.VerdictFinding},
		{"invalid request refused as invalid", rejection, awsmodel.RequestResult{Status: 400, Code: "ValidationError"}, awsmodel.VerdictPass},
		{"invalid request refused for a missing member", rejection, awsmodel.RequestResult{Status: 400, Code: "MissingParameter"}, awsmodel.VerdictPass},
		{"invalid request refused for another reason", rejection, awsmodel.RequestResult{Status: 404, Code: "NoSuchEntity"}, awsmodel.VerdictInconclusive},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			require.Equal(t, test.want, test.request.Judge(test.result))
		})
	}
}

func TestRequestJudgeDiscountsRejectionsOfRefusedBase(t *testing.T) {
	ok := awsmodel.RequestResult{Status: 200}
	invalid := awsmodel.RequestResult{Status: 400, Code: "ValidationError"}
	missing := awsmodel.RequestResult{Status: 400, Code: "MissingParameter"}

	judge := awsmodel.NewRequestJudge()
	require.Equal(t, awsmodel.VerdictPass, judge.Judge(awsmodel.RequestCase{}, ok))
	require.Equal(t, awsmodel.VerdictFinding, judge.Judge(awsmodel.RequestCase{Member: "Tags"}, invalid))
	require.Equal(t, awsmodel.VerdictPass, judge.Judge(awsmodel.RequestCase{Member: "Path"}, ok))
	// Tags alone was refused, so its rejection proves nothing.
	require.Equal(t, awsmodel.VerdictInconclusive, judge.Judge(awsmodel.RequestCase{Member: "Tags", Constraint: awsmodel.ConstraintLength}, invalid))
	require.Equal(t, awsmodel.VerdictPass, judge.Judge(awsmodel.RequestCase{Member: "Path", Constraint: awsmodel.ConstraintPattern}, invalid))
	require.Equal(t, awsmodel.VerdictFinding, judge.Judge(awsmodel.RequestCase{Member: "Path", Constraint: awsmodel.ConstraintPattern}, ok))

	// A refused base leaves nothing attributable to any single member.
	judge = awsmodel.NewRequestJudge()
	require.Equal(t, awsmodel.VerdictInconclusive, judge.Judge(awsmodel.RequestCase{}, missing))
	require.Equal(t, awsmodel.VerdictInconclusive, judge.Judge(awsmodel.RequestCase{Constraint: awsmodel.ConstraintRequired, Path: "$.RoleName"}, invalid))
	require.Equal(t, awsmodel.VerdictInconclusive, judge.Judge(awsmodel.RequestCase{Member: "Tags"}, invalid))
	require.Equal(t, awsmodel.VerdictInconclusive, judge.Judge(awsmodel.RequestCase{Member: "Tags", Constraint: awsmodel.ConstraintLength}, ok))
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
