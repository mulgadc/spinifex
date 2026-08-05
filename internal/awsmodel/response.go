package awsmodel

import (
	"bytes"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"strings"
)

// ValidateResponse decodes an AWS wire response and validates it against the
// operation's output shape. JSON services are decoded directly; EC2 and Query
// XML responses are normalized from their wire names and result wrappers to
// api-2.json member names first.
func ValidateResponse(service Service, operationName string, body []byte) ([]Violation, error) {
	model, err := Load(service)
	if err != nil {
		return nil, err
	}
	operation, ok := model.Operation(operationName)
	if !ok {
		return nil, fmt.Errorf("awsmodel: %s operation %q is not modelled", service, operationName)
	}
	var document any
	switch model.metadata.Protocol {
	case "json", "rest-json":
		document, err = decodeJSONResponse(body)
	case "ec2", "query":
		if operation.Output == nil {
			err = validateEmptyXMLResponse(operationName, body)
		} else {
			document, err = model.decodeXMLResponse(operationName, operation.Output, body)
		}
	default:
		return nil, fmt.Errorf("awsmodel: response decoding for %s protocol %q is not implemented", service, model.metadata.Protocol)
	}
	if err != nil {
		return nil, fmt.Errorf("awsmodel: decode %s %s response: %w", service, operationName, err)
	}
	if operation.Output == nil {
		// Some Query/EC2 operations deliberately have no modelled output. There
		// is no shape to inspect, but decoding above still verifies that the
		// response is syntactically valid and has the correct XML envelope.
		return nil, nil
	}
	return Validate(service, operationName, document)
}

func decodeJSONResponse(body []byte) (any, error) {
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	var document any
	if err := decoder.Decode(&document); err != nil {
		return nil, err
	}
	if err := decoder.Decode(new(any)); !errors.Is(err, io.EOF) {
		if err == nil {
			return nil, errors.New("multiple JSON values")
		}
		return nil, err
	}
	return document, nil
}

type xmlNode struct {
	name     string
	text     strings.Builder
	children []*xmlNode
}

func parseXML(body []byte) (*xmlNode, error) {
	decoder := xml.NewDecoder(bytes.NewReader(body))
	var root *xmlNode
	var stack []*xmlNode
	for {
		token, err := decoder.Token()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, err
		}
		switch token := token.(type) {
		case xml.StartElement:
			node := &xmlNode{name: token.Name.Local}
			if len(stack) == 0 {
				if root != nil {
					return nil, errors.New("multiple XML roots")
				}
				root = node
			} else {
				parent := stack[len(stack)-1]
				parent.children = append(parent.children, node)
			}
			stack = append(stack, node)
		case xml.CharData:
			if len(stack) != 0 {
				stack[len(stack)-1].text.Write([]byte(token))
			}
		case xml.EndElement:
			if len(stack) == 0 {
				return nil, errors.New("unexpected XML end element")
			}
			stack = stack[:len(stack)-1]
		}
	}
	if root == nil {
		return nil, errors.New("empty XML response")
	}
	return root, nil
}

func validateEmptyXMLResponse(operationName string, body []byte) error {
	root, err := parseXML(body)
	if err != nil {
		return err
	}
	wantRoot := operationName + "Response"
	if root.name != wantRoot {
		return fmt.Errorf("root element is %q, want %q", root.name, wantRoot)
	}
	return nil
}

func (m *Model) decodeXMLResponse(operationName string, output *ShapeRef, body []byte) (any, error) {
	root, err := parseXML(body)
	if err != nil {
		return nil, err
	}
	wantRoot := operationName + "Response"
	if root.name != wantRoot {
		return nil, fmt.Errorf("root element is %q, want %q", root.name, wantRoot)
	}

	outputNode := root
	if output.ResultWrapper != "" {
		outputNode = firstXMLChild(root, output.ResultWrapper)
		if outputNode == nil {
			return nil, fmt.Errorf("result wrapper %q is missing", output.ResultWrapper)
		}
	}
	return m.normalizeXMLShape(output.Shape, []*xmlNode{outputNode}, false), nil
}

func (m *Model) normalizeXMLShape(shapeName string, nodes []*xmlNode, flattened bool) any {
	shape := m.shapes[shapeName]
	if shape == nil || len(nodes) == 0 {
		return nil
	}

	switch shape.Type {
	case "structure":
		return m.normalizeXMLStructure(shape, nodes[0])
	case "list":
		itemNodes := nodes
		if !flattened && !shape.Flattened {
			itemName := shape.Member.LocationName
			if itemName == "" {
				if m.metadata.Protocol == "ec2" {
					itemName = "item"
				} else {
					itemName = "member"
				}
			}
			itemNodes = xmlChildren(nodes[0], itemName)
		}
		items := make([]any, 0, len(itemNodes))
		for _, node := range itemNodes {
			items = append(items, m.normalizeXMLShape(shape.Member.Shape, []*xmlNode{node}, false))
		}
		return items
	case "map":
		entries := nodes
		if !flattened && !shape.Flattened {
			entries = xmlChildren(nodes[0], "entry")
		}
		result := make(map[string]any, len(entries))
		for _, entry := range entries {
			keyNode := firstXMLChild(entry, xmlRefName("key", *shape.Key))
			valueNode := firstXMLChild(entry, xmlRefName("value", *shape.Value))
			if keyNode != nil && valueNode != nil {
				key := strings.TrimSpace(keyNode.text.String())
				result[key] = m.normalizeXMLShape(shape.Value.Shape, []*xmlNode{valueNode}, false)
			}
		}
		return result
	default:
		return strings.TrimSpace(nodes[0].text.String())
	}
}

func (m *Model) normalizeXMLStructure(shape *Shape, node *xmlNode) map[string]any {
	document := make(map[string]any)
	wireMembers := make(map[string]string, len(shape.Members))
	for name, ref := range shape.Members {
		wireMembers[xmlRefName(name, ref)] = name
	}

	grouped := make(map[string][]*xmlNode)
	for _, child := range node.children {
		if isTransportMetadata(child.name) {
			continue
		}
		grouped[child.name] = append(grouped[child.name], child)
	}
	for wireName, children := range grouped {
		name, ok := wireMembers[wireName]
		if !ok {
			document[wireName] = strings.TrimSpace(children[0].text.String())
			continue
		}
		ref := shape.Members[name]
		target := m.shapes[ref.Shape]
		flattened := ref.Flattened || (target != nil && target.Flattened)
		document[name] = m.normalizeXMLShape(ref.Shape, children, flattened)
	}
	return document
}

func xmlRefName(memberName string, ref ShapeRef) string {
	if ref.LocationName != "" {
		return ref.LocationName
	}
	return memberName
}

func firstXMLChild(node *xmlNode, name string) *xmlNode {
	for _, child := range node.children {
		if child.name == name {
			return child
		}
	}
	return nil
}

func xmlChildren(node *xmlNode, name string) []*xmlNode {
	children := make([]*xmlNode, 0)
	for _, child := range node.children {
		if child.name == name {
			children = append(children, child)
		}
	}
	return children
}

func isTransportMetadata(name string) bool {
	normalized := strings.ToLower(name)
	return normalized == "requestid" || normalized == "responsemetadata"
}
