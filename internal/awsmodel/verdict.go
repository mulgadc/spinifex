package awsmodel

import (
	"net/http"
	"slices"
	"strings"
)

// Verdict is the outcome of one generated request.
type Verdict string

const (
	VerdictPass         Verdict = "pass"
	VerdictFinding      Verdict = "finding"
	VerdictInconclusive Verdict = "inconclusive"
	// VerdictUndeclaredError is a rejection refused with a validation code
	// that neither the operation nor the common errors declare.
	VerdictUndeclaredError Verdict = "undeclared_error"
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

// commonValidationCodes are validation errors AWS lists as common to every
// service rather than declaring them on each operation.
var commonValidationCodes = map[string]bool{
	"InvalidParameterCombination": true,
	"InvalidParameterValue":       true,
	"MissingParameter":            true,
	"ValidationError":             true,
	"ValidationException":         true,
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

func (r RequestResult) succeeded() bool { return r.Status >= 200 && r.Status < 300 }

// conditionalRequirementCodes name a member the request omits or a pairing it
// breaks. Models mark conditionally required members optional, so an
// acceptance request refused with one of these may be refused by AWS too.
var conditionalRequirementCodes = map[string]bool{
	"InvalidParameterCombination": true,
	"MissingParameter":            true,
}

// RequestJudge judges a plan's cases in plan order. A refusal counts for a
// rejection case only if the acceptance cases it builds on were accepted; a
// rejection case that succeeds is a finding regardless.
type RequestJudge struct {
	declared []string
	refused  map[string]bool
}

func (p RequestPlan) NewJudge() *RequestJudge {
	return &RequestJudge{declared: p.DeclaredErrors, refused: map[string]bool{}}
}

func (j *RequestJudge) Judge(c RequestCase, result RequestResult) Verdict {
	validation := !result.succeeded() && IsValidationErrorCode(result.Code)
	if c.Acceptance() {
		baseRefused := c.Member != "" && j.refused[""]
		j.refused[c.Member] = validation
		switch {
		case baseRefused:
			return VerdictInconclusive
		case result.succeeded():
			return VerdictPass
		case validation && !conditionalRequirementCodes[result.Code]:
			return VerdictFinding
		default:
			return VerdictInconclusive
		}
	}
	if result.succeeded() {
		return VerdictFinding
	}
	// A member with no acceptance case, such as a pagination token, gives a
	// refusal nothing to be attributed against.
	baseRefused, baseJudged := j.refused[""]
	memberRefused, memberJudged := j.refused[c.Member]
	switch {
	case !validation, !baseJudged, !memberJudged, baseRefused, memberRefused:
		return VerdictInconclusive
	case len(j.declared) > 0 && !slices.Contains(j.declared, result.Code) && !commonValidationCodes[result.Code]:
		return VerdictUndeclaredError
	default:
		return VerdictPass
	}
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
