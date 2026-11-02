package awsmodel

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"maps"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"
)

// EncodedRequest is an operation input serialized for the service's protocol,
// ready to be addressed to an endpoint and signed.
type EncodedRequest struct {
	Method string
	// Path is escaped; it may carry a query string of its own.
	Path   string
	Query  url.Values
	Header http.Header
	Body   []byte
}

// URL joins the request's path and query onto endpoint.
func (r *EncodedRequest) URL(endpoint string) string {
	target := strings.TrimSuffix(endpoint, "/") + r.Path
	if len(r.Query) == 0 {
		return target
	}
	separator := "?"
	if strings.Contains(r.Path, "?") {
		separator = "&"
	}
	return target + separator + r.Query.Encode()
}

// EncodeRequest serializes input, as produced by GenerateRequests, for the
// service's protocol: query, ec2, json, rest-json or rest-xml.
func EncodeRequest(service Service, operationName string, input map[string]any) (*EncodedRequest, error) {
	model, err := Load(service)
	if err != nil {
		return nil, err
	}
	operation, ok := model.Operation(operationName)
	if !ok {
		return nil, fmt.Errorf("awsmodel: %s operation %q is not modelled", service, operationName)
	}
	encoder := requestEncoder{model: model}
	switch model.metadata.Protocol {
	case "query", "ec2":
		return encoder.query(operation, input)
	case "json":
		return encoder.json(operation, input)
	case "rest-json":
		return encoder.restJSON(operation, input)
	case "rest-xml":
		return encoder.restXML(operation, input)
	default:
		return nil, fmt.Errorf("awsmodel: encoding %s requests is not implemented", model.metadata.Protocol)
	}
}

type requestEncoder struct {
	model *Model
}

func (e requestEncoder) query(operation *Operation, input map[string]any) (*EncodedRequest, error) {
	values := url.Values{
		"Action":  {operation.Name},
		"Version": {e.model.metadata.APIVersion},
	}
	if operation.Input != nil {
		if err := e.queryStructure(values, "", operation.Input.Shape, input); err != nil {
			return nil, err
		}
	}
	return &EncodedRequest{
		Method: http.MethodPost,
		Path:   "/",
		Header: http.Header{"Content-Type": {"application/x-www-form-urlencoded; charset=utf-8"}},
		Body:   []byte(values.Encode()),
	}, nil
}

func (e requestEncoder) queryStructure(values url.Values, prefix, shapeName string, input map[string]any) error {
	shape := e.model.shapes[shapeName]
	for _, member := range slices.Sorted(maps.Keys(input)) {
		ref, ok := shape.Members[member]
		if !ok {
			return fmt.Errorf("%s has no member %q", shapeName, member)
		}
		if err := e.queryValue(values, prefix+e.queryName(member, ref), ref, input[member]); err != nil {
			return err
		}
	}
	return nil
}

// queryName is the parameter name for a member: the ec2 protocol capitalises
// its XML name unless ec2QueryName gives one; awsQuery uses the XML name.
func (e requestEncoder) queryName(member string, ref ShapeRef) string {
	if e.model.metadata.Protocol != "ec2" {
		if ref.LocationName != "" {
			return ref.LocationName
		}
		return member
	}
	if ref.QueryName != "" {
		return ref.QueryName
	}
	name := member
	if ref.LocationName != "" {
		name = ref.LocationName
	}
	runes := []rune(name)
	runes[0] = unicode.ToUpper(runes[0])
	return string(runes)
}

func (e requestEncoder) queryValue(values url.Values, name string, ref ShapeRef, value any) error {
	shape := e.model.shapes[ref.Shape]
	switch shape.Type {
	case "structure":
		fields, ok := value.(map[string]any)
		if !ok {
			return fmt.Errorf("%s: want a structure, got %T", name, value)
		}
		return e.queryStructure(values, name+".", ref.Shape, fields)
	case "list":
		items, ok := value.([]any)
		if !ok {
			return fmt.Errorf("%s: want a list, got %T", name, value)
		}
		if len(items) == 0 {
			// awsQuery sends an empty list as an empty parameter; ec2 cannot.
			if e.model.metadata.Protocol != "ec2" {
				values.Set(name, "")
			}
			return nil
		}
		itemPrefix := name + "."
		if e.model.metadata.Protocol != "ec2" && !ref.Flattened && !shape.Flattened {
			memberName := "member"
			if shape.Member.LocationName != "" {
				memberName = shape.Member.LocationName
			}
			itemPrefix += memberName + "."
		}
		for i, item := range items {
			if err := e.queryValue(values, itemPrefix+strconv.Itoa(i+1), *shape.Member, item); err != nil {
				return err
			}
		}
		return nil
	case "map":
		entries, ok := value.(map[string]any)
		if !ok {
			return fmt.Errorf("%s: want a map, got %T", name, value)
		}
		entryPrefix := name + "."
		if e.model.metadata.Protocol != "ec2" && !ref.Flattened && !shape.Flattened {
			entryPrefix += "entry."
		}
		keyName, valueName := "key", "value"
		if shape.Key.LocationName != "" {
			keyName = shape.Key.LocationName
		}
		if shape.Value.LocationName != "" {
			valueName = shape.Value.LocationName
		}
		for i, key := range slices.Sorted(maps.Keys(entries)) {
			entry := entryPrefix + strconv.Itoa(i+1) + "."
			values.Set(entry+keyName, key)
			if err := e.queryValue(values, entry+valueName, *shape.Value, entries[key]); err != nil {
				return err
			}
		}
		return nil
	default:
		scalar, err := scalarString(value, ref, shape, "iso8601")
		if err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
		values.Set(name, scalar)
		return nil
	}
}

func (e requestEncoder) json(operation *Operation, input map[string]any) (*EncodedRequest, error) {
	body := []byte("{}")
	if operation.Input != nil {
		document, err := e.jsonValue(*operation.Input, input)
		if err != nil {
			return nil, err
		}
		if body, err = json.Marshal(document); err != nil {
			return nil, err
		}
	}
	metadata := e.model.metadata
	return &EncodedRequest{
		Method: http.MethodPost,
		Path:   "/",
		Header: http.Header{
			"Content-Type": {"application/x-amz-json-" + metadata.JSONVersion},
			"X-Amz-Target": {metadata.TargetPrefix + "." + operation.Name},
		},
		Body: body,
	}, nil
}

// jsonValue converts a generated value into its JSON document form.
func (e requestEncoder) jsonValue(ref ShapeRef, value any) (any, error) {
	shape := e.model.shapes[ref.Shape]
	switch shape.Type {
	case "structure":
		fields, ok := value.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("%s: want a structure, got %T", ref.Shape, value)
		}
		document := make(map[string]any, len(fields))
		for member, field := range fields {
			memberRef, ok := shape.Members[member]
			if !ok {
				return nil, fmt.Errorf("%s has no member %q", ref.Shape, member)
			}
			encoded, err := e.jsonValue(memberRef, field)
			if err != nil {
				return nil, err
			}
			document[member] = encoded
		}
		return document, nil
	case "list":
		items, ok := value.([]any)
		if !ok {
			return nil, fmt.Errorf("%s: want a list, got %T", ref.Shape, value)
		}
		document := make([]any, len(items))
		for i, item := range items {
			encoded, err := e.jsonValue(*shape.Member, item)
			if err != nil {
				return nil, err
			}
			document[i] = encoded
		}
		return document, nil
	case "map":
		entries, ok := value.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("%s: want a map, got %T", ref.Shape, value)
		}
		document := make(map[string]any, len(entries))
		for key, entry := range entries {
			encoded, err := e.jsonValue(*shape.Value, entry)
			if err != nil {
				return nil, err
			}
			document[key] = encoded
		}
		return document, nil
	case "timestamp":
		stamp, ok := value.(time.Time)
		if !ok {
			return nil, fmt.Errorf("%s: want a timestamp, got %T", ref.Shape, value)
		}
		if format := timestampFormatFor(ref, shape, "unixTimestamp"); format != "unixTimestamp" {
			return formatTimestamp(stamp, format), nil
		}
		return stamp.Unix(), nil
	case "blob":
		data, ok := value.([]byte)
		if !ok {
			return nil, fmt.Errorf("%s: want a blob, got %T", ref.Shape, value)
		}
		return base64.StdEncoding.EncodeToString(data), nil
	default:
		return value, nil
	}
}

func (e requestEncoder) restJSON(operation *Operation, input map[string]any) (*EncodedRequest, error) {
	request, members, err := e.restBindings(operation, input)
	if err != nil || len(members) == 0 {
		return request, err
	}
	shape := e.model.shapes[operation.Input.Shape]
	var document any
	if payload, ok := members[shape.Payload]; ok && shape.Payload != "" {
		if document, err = e.jsonValue(shape.Members[shape.Payload], payload); err != nil {
			return nil, err
		}
	} else {
		body := make(map[string]any, len(members))
		for member, value := range members {
			ref := shape.Members[member]
			encoded, err := e.jsonValue(ref, value)
			if err != nil {
				return nil, err
			}
			body[elementName(member, ref)] = encoded
		}
		document = body
	}
	if request.Body, err = json.Marshal(document); err != nil {
		return nil, err
	}
	request.Header.Set("Content-Type", "application/json")
	return request, nil
}

// restBindings places the URI, query string and header members of input on a
// new request, and returns the members left for the body.
func (e requestEncoder) restBindings(operation *Operation, input map[string]any) (*EncodedRequest, map[string]any, error) {
	request := &EncodedRequest{Method: operation.HTTP.Method, Query: url.Values{}, Header: http.Header{}}
	path := operation.HTTP.RequestURI
	if operation.Input == nil {
		request.Path = path
		return request, nil, nil
	}
	shape := e.model.shapes[operation.Input.Shape]
	body := map[string]any{}
	for _, member := range slices.Sorted(maps.Keys(input)) {
		ref, ok := shape.Members[member]
		if !ok {
			return nil, nil, fmt.Errorf("%s has no member %q", operation.Input.Shape, member)
		}
		value := input[member]
		target := e.model.shapes[ref.Shape]
		switch ref.Location {
		case "uri":
			text, err := scalarString(value, ref, target, "iso8601")
			if err != nil {
				return nil, nil, fmt.Errorf("%s: %w", member, err)
			}
			path = strings.Replace(path, "{"+member+"+}", escapeGreedyLabel(text), 1)
			path = strings.Replace(path, "{"+member+"}", url.PathEscape(text), 1)
		case "querystring":
			if err := e.restQuery(request.Query, ref, target, value); err != nil {
				return nil, nil, fmt.Errorf("%s: %w", member, err)
			}
		case "header":
			text, err := e.headerValue(ref, target, value)
			if err != nil {
				return nil, nil, fmt.Errorf("%s: %w", member, err)
			}
			request.Header.Set(ref.LocationName, text)
		case "headers":
			entries, _ := value.(map[string]any)
			for key, entry := range entries {
				request.Header.Set(ref.LocationName+key, fmt.Sprint(entry))
			}
		default:
			body[member] = value
		}
	}
	request.Path = path
	return request, body, nil
}

// restXML encodes the body as AWS SDKs do: a root element named for the input
// shape, or for the payload member, in the service's namespace. An input with
// body members sends its root element even when none of them is set.
func (e requestEncoder) restXML(operation *Operation, input map[string]any) (*EncodedRequest, error) {
	request, members, err := e.restBindings(operation, input)
	if err != nil || operation.Input == nil {
		return request, err
	}
	shape := e.model.shapes[operation.Input.Shape]
	var root *xmlElement
	switch {
	case shape.Payload != "":
		value, ok := members[shape.Payload]
		if !ok {
			return request, nil
		}
		ref := shape.Members[shape.Payload]
		target := e.model.shapes[ref.Shape]
		if target.Type != "structure" {
			switch value := value.(type) {
			case []byte:
				request.Body = value
			case string:
				request.Body = []byte(value)
			default:
				return nil, fmt.Errorf("%s: want a blob or string payload, got %T", shape.Payload, value)
			}
			return request, nil
		}
		name := ref.LocationName
		if name == "" {
			name = elementName(ref.Shape, ShapeRef{LocationName: target.LocationName})
		}
		if root, err = e.xmlRoot(name, ref, value); err != nil {
			return nil, err
		}
	case hasBodyMembers(shape):
		name := elementName(operation.Input.Shape, ShapeRef{LocationName: shape.LocationName})
		if root, err = e.xmlRoot(name, *operation.Input, members); err != nil {
			return nil, err
		}
	default:
		return request, nil
	}
	var body bytes.Buffer
	encoder := xml.NewEncoder(&body)
	if err := root.encode(encoder); err != nil {
		return nil, err
	}
	if err := encoder.Flush(); err != nil {
		return nil, err
	}
	request.Body = body.Bytes()
	request.Header.Set("Content-Type", "application/xml")
	return request, nil
}

func hasBodyMembers(shape *Shape) bool {
	for _, ref := range shape.Members {
		if ref.Location == "" {
			return true
		}
	}
	return false
}

// xmlRoot is the structure value encoded under name, in its shape's namespace
// or else the service's.
func (e requestEncoder) xmlRoot(name string, ref ShapeRef, value any) (*xmlElement, error) {
	holder := &xmlElement{}
	if err := e.xmlValue(holder, name, ref, value); err != nil {
		return nil, err
	}
	root := holder.children[0]
	if namespace := e.model.metadata.XMLNamespace; namespace.URI != "" && e.model.shapes[ref.Shape].XMLNamespace.URI == "" {
		root.attrs = append([]xml.Attr{namespaceAttr(namespace)}, root.attrs...)
	}
	return root, nil
}

func (e requestEncoder) xmlStructure(parent *xmlElement, shapeName string, fields map[string]any) error {
	shape := e.model.shapes[shapeName]
	for _, member := range slices.Sorted(maps.Keys(fields)) {
		ref, ok := shape.Members[member]
		if !ok {
			return fmt.Errorf("%s has no member %q", shapeName, member)
		}
		name := elementName(member, ref)
		if ref.XMLAttribute {
			text, err := scalarString(fields[member], ref, e.model.shapes[ref.Shape], "iso8601")
			if err != nil {
				return fmt.Errorf("%s: %w", name, err)
			}
			parent.attrs = append(parent.attrs, xml.Attr{Name: xml.Name{Local: name}, Value: text})
			continue
		}
		if err := e.xmlValue(parent, name, ref, fields[member]); err != nil {
			return err
		}
	}
	return nil
}

// xmlValue appends the elements value is encoded as to parent. A list or map
// is wrapped in an element named name unless it is flattened.
func (e requestEncoder) xmlValue(parent *xmlElement, name string, ref ShapeRef, value any) error {
	shape := e.model.shapes[ref.Shape]
	switch shape.Type {
	case "structure":
		fields, ok := value.(map[string]any)
		if !ok {
			return fmt.Errorf("%s: want a structure, got %T", name, value)
		}
		element := parent.add(name)
		if shape.XMLNamespace.URI != "" {
			element.attrs = append(element.attrs, namespaceAttr(shape.XMLNamespace))
		}
		return e.xmlStructure(element, ref.Shape, fields)
	case "list":
		items, ok := value.([]any)
		if !ok {
			return fmt.Errorf("%s: want a list, got %T", name, value)
		}
		container, itemName := parent, name
		if !ref.Flattened {
			container, itemName = parent.add(name), elementName("member", *shape.Member)
		}
		for _, item := range items {
			if err := e.xmlValue(container, itemName, *shape.Member, item); err != nil {
				return err
			}
		}
		return nil
	case "map":
		entries, ok := value.(map[string]any)
		if !ok {
			return fmt.Errorf("%s: want a map, got %T", name, value)
		}
		container, entryName := parent, name
		if !ref.Flattened {
			container, entryName = parent.add(name), "entry"
		}
		keyName, valueName := elementName("key", *shape.Key), elementName("value", *shape.Value)
		for _, key := range slices.Sorted(maps.Keys(entries)) {
			entry := container.add(entryName)
			entry.add(keyName).text = key
			if err := e.xmlValue(entry, valueName, *shape.Value, entries[key]); err != nil {
				return err
			}
		}
		return nil
	default:
		text, err := scalarString(value, ref, shape, "iso8601")
		if err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
		parent.add(name).text = text
		return nil
	}
}

// elementName is a member's wire name: its xmlName or jsonName, else its own.
func elementName(member string, ref ShapeRef) string {
	if ref.LocationName != "" {
		return ref.LocationName
	}
	return member
}

func namespaceAttr(namespace XMLNamespace) xml.Attr {
	name := "xmlns"
	if namespace.Prefix != "" {
		name += ":" + namespace.Prefix
	}
	return xml.Attr{Name: xml.Name{Local: name}, Value: namespace.URI}
}

// xmlElement is one element of an encoded rest-xml body.
type xmlElement struct {
	name     string
	attrs    []xml.Attr
	text     string
	children []*xmlElement
}

func (x *xmlElement) add(name string) *xmlElement {
	child := &xmlElement{name: name}
	x.children = append(x.children, child)
	return child
}

func (x *xmlElement) encode(encoder *xml.Encoder) error {
	start := xml.StartElement{Name: xml.Name{Local: x.name}, Attr: x.attrs}
	if err := encoder.EncodeToken(start); err != nil {
		return err
	}
	if x.text != "" {
		if err := encoder.EncodeToken(xml.CharData(x.text)); err != nil {
			return err
		}
	}
	for _, child := range x.children {
		if err := child.encode(encoder); err != nil {
			return err
		}
	}
	return encoder.EncodeToken(start.End())
}

func (e requestEncoder) restQuery(query url.Values, ref ShapeRef, shape *Shape, value any) error {
	switch shape.Type {
	case "list":
		items, _ := value.([]any)
		for _, item := range items {
			text, err := scalarString(item, *shape.Member, e.model.shapes[shape.Member.Shape], "iso8601")
			if err != nil {
				return err
			}
			query.Add(ref.LocationName, text)
		}
		return nil
	case "map":
		entries, _ := value.(map[string]any)
		for key, entry := range entries {
			query.Add(key, fmt.Sprint(entry))
		}
		return nil
	default:
		text, err := scalarString(value, ref, shape, "iso8601")
		if err != nil {
			return err
		}
		query.Set(ref.LocationName, text)
		return nil
	}
}

func (e requestEncoder) headerValue(ref ShapeRef, shape *Shape, value any) (string, error) {
	if shape.Type == "list" {
		items, _ := value.([]any)
		texts := make([]string, len(items))
		for i, item := range items {
			text, err := scalarString(item, *shape.Member, e.model.shapes[shape.Member.Shape], "rfc822")
			if err != nil {
				return "", err
			}
			texts[i] = text
		}
		return strings.Join(texts, ","), nil
	}
	return scalarString(value, ref, shape, "rfc822")
}

// escapeGreedyLabel escapes a greedy label's segments but keeps its slashes.
func escapeGreedyLabel(value string) string {
	segments := strings.Split(value, "/")
	for i, segment := range segments {
		segments[i] = url.PathEscape(segment)
	}
	return strings.Join(segments, "/")
}

func scalarString(value any, ref ShapeRef, shape *Shape, defaultTimestamp string) (string, error) {
	switch value := value.(type) {
	case string:
		return value, nil
	case bool:
		return strconv.FormatBool(value), nil
	case int64:
		return strconv.FormatInt(value, 10), nil
	case float64:
		return strconv.FormatFloat(value, 'g', -1, 64), nil
	case []byte:
		return base64.StdEncoding.EncodeToString(value), nil
	case time.Time:
		return formatTimestamp(value, timestampFormatFor(ref, shape, defaultTimestamp)), nil
	default:
		return "", fmt.Errorf("unsupported scalar %T", value)
	}
}

func timestampFormatFor(ref ShapeRef, shape *Shape, defaultFormat string) string {
	if ref.TimestampFormat != "" {
		return ref.TimestampFormat
	}
	if shape.TimestampFormat != "" {
		return shape.TimestampFormat
	}
	return defaultFormat
}

func formatTimestamp(value time.Time, format string) string {
	switch format {
	case "rfc822":
		return value.UTC().Format(http.TimeFormat)
	case "unixTimestamp":
		return strconv.FormatInt(value.Unix(), 10)
	default:
		return value.UTC().Format(time.RFC3339)
	}
}
