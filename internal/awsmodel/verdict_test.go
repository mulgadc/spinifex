package awsmodel

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestRequestCaseJudge(t *testing.T) {
	acceptance := RequestCase{Operation: "CreateRole"}
	rejection := RequestCase{Operation: "CreateRole", Constraint: ConstraintLength, Path: "$.RoleName"}
	tests := []struct {
		name    string
		request RequestCase
		result  RequestResult
		want    Verdict
	}{
		{"accepted valid request", acceptance, RequestResult{Status: 200}, VerdictPass},
		{"valid request refused as invalid", acceptance, RequestResult{Status: 400, Code: "ValidationError"}, VerdictFinding},
		{"valid request refused as malformed", acceptance, RequestResult{Status: 400, Code: "InvalidInstanceID.Malformed"}, VerdictFinding},
		{"valid request missing a conditionally required member", acceptance, RequestResult{Status: 400, Code: "MissingParameter"}, VerdictInconclusive},
		{"valid request names a missing resource", acceptance, RequestResult{Status: 404, Code: "NoSuchEntity"}, VerdictInconclusive},
		{"valid request times out", acceptance, RequestResult{Code: "Timeout"}, VerdictInconclusive},
		{"invalid request accepted", rejection, RequestResult{Status: 200}, VerdictFinding},
		{"invalid request refused as invalid", rejection, RequestResult{Status: 400, Code: "ValidationError"}, VerdictPass},
		{"invalid request refused for a missing member", rejection, RequestResult{Status: 400, Code: "MissingParameter"}, VerdictPass},
		{"invalid request refused for another reason", rejection, RequestResult{Status: 404, Code: "NoSuchEntity"}, VerdictInconclusive},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			require.Equal(t, test.want, test.request.Judge(test.result))
		})
	}
}

func TestRequestJudgeDiscountsRejectionsOfRefusedBase(t *testing.T) {
	ok := RequestResult{Status: 200}
	invalid := RequestResult{Status: 400, Code: "ValidationError"}
	missing := RequestResult{Status: 400, Code: "MissingParameter"}

	judge := NewRequestJudge()
	require.Equal(t, VerdictPass, judge.Judge(RequestCase{}, ok))
	require.Equal(t, VerdictFinding, judge.Judge(RequestCase{Member: "Tags"}, invalid))
	require.Equal(t, VerdictPass, judge.Judge(RequestCase{Member: "Path"}, ok))
	// Tags alone was refused, so its rejection proves nothing.
	require.Equal(t, VerdictInconclusive, judge.Judge(RequestCase{Member: "Tags", Constraint: ConstraintLength}, invalid))
	require.Equal(t, VerdictPass, judge.Judge(RequestCase{Member: "Path", Constraint: ConstraintPattern}, invalid))
	require.Equal(t, VerdictFinding, judge.Judge(RequestCase{Member: "Path", Constraint: ConstraintPattern}, ok))

	// A refused base leaves nothing attributable to any single member.
	judge = NewRequestJudge()
	require.Equal(t, VerdictInconclusive, judge.Judge(RequestCase{}, missing))
	require.Equal(t, VerdictInconclusive, judge.Judge(RequestCase{Constraint: ConstraintRequired, Path: "$.RoleName"}, invalid))
	require.Equal(t, VerdictInconclusive, judge.Judge(RequestCase{Member: "Tags"}, invalid))
	require.Equal(t, VerdictInconclusive, judge.Judge(RequestCase{Member: "Tags", Constraint: ConstraintLength}, ok))
}

func TestDecodeRequestResult(t *testing.T) {
	tests := []struct {
		name    string
		service Service
		header  http.Header
		body    string
		code    string
		message string
	}{
		{"awsQuery", IAM, nil,
			`<ErrorResponse><Error><Type>Sender</Type><Code>ValidationError</Code><Message>bad name</Message></Error></ErrorResponse>`,
			"ValidationError", "bad name"},
		{"ec2", EC2, nil,
			`<Response><Errors><Error><Code>InvalidParameterValue</Code><Message>bad id</Message></Error></Errors><RequestID>r</RequestID></Response>`,
			"InvalidParameterValue", "bad id"},
		{"json", ECS, nil,
			`{"__type":"com.amazonaws.ecs#InvalidParameterException","message":"bad cluster"}`,
			"InvalidParameterException", "bad cluster"},
		{"rest-json header", EKS, http.Header{"X-Amzn-Errortype": {"InvalidParameterException:http://internal.amazon.com/"}},
			`{"message":"bad name"}`,
			"InvalidParameterException", "bad name"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			result := DecodeRequestResult(test.service, 400, test.header, []byte(test.body))
			require.Equal(t, RequestResult{Status: 400, Code: test.code, Message: test.message}, result)
		})
	}
	require.Equal(t, RequestResult{Status: 200}, DecodeRequestResult(IAM, 200, nil, []byte("<ok/>")))
}
