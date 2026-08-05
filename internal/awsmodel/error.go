package awsmodel

import (
	"bytes"
	"encoding/json"
	"fmt"
	"slices"
	"sort"
	"strings"
)

// ValidateErrorResponse decodes an AWS error envelope and checks its code
// against the errors declared for operation. Modelled is false when AWS's
// operation definition declares no errors, as is the case for some STS calls.
func ValidateErrorResponse(service Service, operationName string, body []byte) (violations []Violation, modelled bool, err error) {
	model, err := Load(service)
	if err != nil {
		return nil, false, err
	}
	operation, ok := model.Operation(operationName)
	if !ok {
		return nil, false, fmt.Errorf("awsmodel: %s operation %q is not modelled", service, operationName)
	}

	code, err := model.decodeErrorCode(body)
	if err != nil {
		return nil, false, fmt.Errorf("awsmodel: decode %s %s error response: %w", service, operationName, err)
	}
	if isUnmodelledCommonError(service, code) {
		return nil, false, nil
	}
	codes := model.operationErrorCodes(operation)
	if len(codes) == 0 {
		return nil, false, nil
	}
	if slices.Contains(codes, code) {
		return nil, true, nil
	}
	return []Violation{{
		Rule:    RuleErrorCode,
		Path:    "$error.Code",
		Message: fmt.Sprintf("error code %q is not declared; allowed [%s]", code, strings.Join(codes, ", ")),
	}}, true, nil
}

// isUnmodelledCommonError identifies service/protocol errors AWS does not put
// in operation error lists. They remain counted as unmodelled by the harness;
// they are not treated as proof that an operation-specific code conforms.
func isUnmodelledCommonError(service Service, code string) bool {
	switch service {
	case IAM, ElasticLoadBalancingV2:
		return isUnmodelledQueryError(code)
	case STS:
		// STS also uses these two runtime input errors even though api-2.json
		// does not declare them on AssumeRole/GetSessionToken.
		return isUnmodelledQueryError(code) || code == "ValidationError" || code == "InvalidParameterValue"
	case ECS:
		switch code {
		case "AccessDeniedException", "IncompleteSignatureException", "InvalidClientTokenIdException",
			"MissingAuthenticationTokenException", "RequestExpiredException", "SignatureDoesNotMatchException",
			"ThrottlingException":
			return true
		}
	default:
	}
	return false
}

func isUnmodelledQueryError(code string) bool {
	switch code {
	case "AccessDenied", "IncompleteSignature", "InvalidClientTokenId", "MissingAuthenticationToken",
		"RequestExpired", "SignatureDoesNotMatch", "Throttling":
		return true
	default:
		return false
	}
}

func (m *Model) operationErrorCodes(operation *Operation) []string {
	codes := make([]string, 0, len(operation.Errors))
	for _, ref := range operation.Errors {
		code := ref.Shape
		if shape := m.shapes[ref.Shape]; shape != nil && shape.Error != nil && shape.Error.Code != "" {
			code = shape.Error.Code
		}
		codes = append(codes, code)
	}
	sort.Strings(codes)
	return codes
}

func (m *Model) decodeErrorCode(body []byte) (string, error) {
	switch m.metadata.Protocol {
	case "json", "rest-json":
		var envelope struct {
			Type string `json:"__type"`
			Code string `json:"code"`
		}
		decoder := json.NewDecoder(bytes.NewReader(body))
		if err := decoder.Decode(&envelope); err != nil {
			return "", err
		}
		code := envelope.Type
		if code == "" {
			code = envelope.Code
		}
		if separator := strings.LastIndex(code, "#"); separator >= 0 {
			code = code[separator+1:]
		}
		if separator := strings.Index(code, ":"); separator >= 0 {
			code = code[:separator]
		}
		if code == "" {
			return "", fmt.Errorf("JSON error envelope has no code")
		}
		return code, nil
	case "ec2", "query":
		root, err := parseXML(body)
		if err != nil {
			return "", err
		}
		code := findXMLText(root, "Code")
		if code == "" {
			return "", fmt.Errorf("XML error envelope has no Code element")
		}
		return code, nil
	default:
		return "", fmt.Errorf("error decoding for protocol %q is not implemented", m.metadata.Protocol)
	}
}

func findXMLText(node *xmlNode, name string) string {
	if node.name == name {
		return strings.TrimSpace(node.text.String())
	}
	for _, child := range node.children {
		if value := findXMLText(child, name); value != "" {
			return value
		}
	}
	return ""
}
