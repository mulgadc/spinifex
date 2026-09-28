package awsmodel

import (
	"net/http"
	"strings"
)

// Verdict is the outcome of one generated request.
type Verdict string

const (
	VerdictPass         Verdict = "pass"
	VerdictFinding      Verdict = "finding"
	VerdictInconclusive Verdict = "inconclusive"
)

// validationErrorCodes are the codes that refuse a request as failing
// parameter validation, as opposed to naming something missing or conflicting
// with state. InvalidParameterValueException is not modelled but is one.
var validationErrorCodes = map[string]bool{
	"InvalidInput":                   true,
	"InvalidParameter":               true,
	"InvalidParameterCombination":    true,
	"InvalidParameterException":      true,
	"InvalidParameterValue":          true,
	"InvalidParameterValueException": true,
	"MissingParameter":               true,
	"SerializationException":         true,
	"UnknownParameter":               true,
	"ValidationError":                true,
	"ValidationException":            true,
}

// IsValidationErrorCode reports whether code is a parameter validation error,
// including EC2's per-resource "*.Malformed" codes.
func IsValidationErrorCode(code string) bool {
	return validationErrorCodes[code] || strings.HasSuffix(code, ".Malformed")
}

// RequestResult is what came back for one generated request. Code is empty for
// a success.
type RequestResult struct {
	Status  int
	Code    string
	Message string
}

// conditionalRequirementCodes name a member the request omits or a pairing it
// breaks. Models mark conditionally required members optional, so an
// acceptance request refused with one of these may be refused by AWS too.
var conditionalRequirementCodes = map[string]bool{
	"InvalidParameterCombination": true,
	"MissingParameter":            true,
}

// Judge decides a generated request's verdict. The acceptance request fails
// only on a validation error; any other error, such as a missing resource,
// says nothing about its parameters. A rejection request passes on a
// validation error and is a finding if it succeeds.
func (c RequestCase) Judge(result RequestResult) Verdict {
	succeeded := result.Status >= 200 && result.Status < 300
	validation := !succeeded && IsValidationErrorCode(result.Code)
	switch {
	case c.Acceptance() && succeeded:
		return VerdictPass
	case c.Acceptance() && validation && !conditionalRequirementCodes[result.Code]:
		return VerdictFinding
	case !c.Acceptance() && succeeded:
		return VerdictFinding
	case !c.Acceptance() && validation:
		return VerdictPass
	default:
		return VerdictInconclusive
	}
}

// RequestJudge judges one operation's cases in plan order, where each case
// builds on the required-members-only acceptance case. Once that base is
// rejected as invalid no later case can be attributed to its member, so the
// base carries any finding and the later cases are inconclusive.
type RequestJudge struct {
	rejected map[string]bool
}

func NewRequestJudge() *RequestJudge {
	return &RequestJudge{rejected: map[string]bool{}}
}

func (j *RequestJudge) Judge(c RequestCase, result RequestResult) Verdict {
	verdict := c.Judge(result)
	if c.Member != "" && j.rejected[""] {
		return VerdictInconclusive
	}
	if c.Acceptance() {
		j.rejected[c.Member] = IsValidationErrorCode(result.Code)
		return verdict
	}
	if j.rejected[c.Member] {
		return VerdictInconclusive
	}
	return verdict
}

// DecodeRequestResult extracts the error code and message from a response.
func DecodeRequestResult(service Service, status int, header http.Header, body []byte) RequestResult {
	result := RequestResult{Status: status}
	if status >= 200 && status < 300 {
		return result
	}
	model, err := Load(service)
	if err != nil {
		return result
	}
	// rest-json services may carry the code only in a header.
	if code := header.Get("X-Amzn-Errortype"); code != "" {
		result.Code = trimErrorCode(code)
	} else if code, err := model.decodeErrorCode(body); err == nil {
		result.Code = code
	}
	result.Message = decodeErrorMessage(model.metadata.Protocol, body)
	return result
}

func trimErrorCode(code string) string {
	if separator := strings.Index(code, ":"); separator >= 0 {
		code = code[:separator]
	}
	if separator := strings.LastIndex(code, "#"); separator >= 0 {
		code = code[separator+1:]
	}
	return code
}

func decodeErrorMessage(protocol string, body []byte) string {
	switch protocol {
	case "json", "rest-json":
		document, err := decodeJSONResponse(body)
		if err != nil {
			return ""
		}
		fields, _ := document.(map[string]any)
		for _, key := range []string{"message", "Message", "errorMessage"} {
			if message, ok := fields[key].(string); ok {
				return message
			}
		}
		return ""
	default:
		root, err := parseXML(body)
		if err != nil {
			return ""
		}
		return findXMLText(root, "Message")
	}
}
