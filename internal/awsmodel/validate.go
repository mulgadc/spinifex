package awsmodel

import (
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"
)

// Rule identifies a model constraint violated by a response document.
type Rule string

const (
	RuleUnknownField   Rule = "unknown_field"
	RuleRequiredMember Rule = "required_member"
	RuleEnum           Rule = "enum"
	RuleErrorCode      Rule = "error_code"
	RuleHTTPStatus     Rule = "http_status"
)

// Violation describes one response value that does not conform to its output
// shape. Path uses a JSONPath-like notation rooted at $.
type Violation struct {
	Rule    Rule
	Path    string
	Message string
}

func (v Violation) String() string {
	return fmt.Sprintf("%s: %s (%s)", v.Path, v.Message, v.Rule)
}

// Validate checks a normalized response document against an operation's
// output shape. Structure keys must use the member names from api-2.json;
// protocol-specific response middleware is responsible for translating wire
// names and wrappers to that representation.
func Validate(service Service, operationName string, document any) ([]Violation, error) {
	model, err := Load(service)
	if err != nil {
		return nil, err
	}
	operation, ok := model.Operation(operationName)
	if !ok {
		return nil, fmt.Errorf("awsmodel: %s operation %q is not modelled", service, operationName)
	}
	if operation.Output == nil {
		return nil, nil
	}

	var violations []Violation
	model.validateShape(operation.Output.Shape, "$", document, &violations)
	return violations, nil
}

func (m *Model) validateShape(shapeName, path string, value any, violations *[]Violation) {
	shape := m.shapes[shapeName]
	if shape == nil {
		// Load verifies references, so this is unreachable for a loaded model.
		return
	}

	switch shape.Type {
	case "structure":
		m.validateStructure(shape, path, value, violations)
	case "list":
		values, ok := value.([]any)
		if !ok {
			return
		}
		for index, item := range values {
			m.validateShape(shape.Member.Shape, fmt.Sprintf("%s[%d]", path, index), item, violations)
		}
	case "map":
		values, ok := value.(map[string]any)
		if !ok {
			return
		}
		keys := slices.Sorted(maps.Keys(values))
		for _, key := range keys {
			keyPath := path + "[" + strconv.Quote(key) + "]"
			m.validateShape(shape.Key.Shape, keyPath+".key", key, violations)
			m.validateShape(shape.Value.Shape, keyPath, values[key], violations)
		}
	case "string":
		stringValue, ok := value.(string)
		if !ok || len(shape.Enum) == 0 {
			return
		}
		if !slices.Contains(shape.Enum, stringValue) {
			*violations = append(*violations, Violation{
				Rule:    RuleEnum,
				Path:    path,
				Message: fmt.Sprintf("value %q is not one of [%s]", stringValue, strings.Join(shape.Enum, ", ")),
			})
		}
	}
}

func (m *Model) validateStructure(shape *Shape, path string, value any, violations *[]Violation) {
	fields, ok := value.(map[string]any)
	if !ok {
		fields = map[string]any{}
	}

	required := append([]string(nil), shape.Required...)
	slices.Sort(required)
	for _, name := range required {
		if _, present := fields[name]; !present {
			*violations = append(*violations, Violation{
				Rule:    RuleRequiredMember,
				Path:    memberPath(path, name),
				Message: "required member is missing",
			})
		}
	}

	fieldNames := slices.Sorted(maps.Keys(fields))
	for _, name := range fieldNames {
		if _, modelled := shape.Members[name]; !modelled {
			*violations = append(*violations, Violation{
				Rule:    RuleUnknownField,
				Path:    memberPath(path, name),
				Message: "field is not present in the model shape",
			})
		}
	}

	memberNames := slices.Sorted(maps.Keys(shape.Members))
	for _, name := range memberNames {
		field, present := fields[name]
		if !present {
			continue
		}
		m.validateShape(shape.Members[name].Shape, memberPath(path, name), field, violations)
	}
}

func memberPath(parent, name string) string {
	return parent + "." + name
}
