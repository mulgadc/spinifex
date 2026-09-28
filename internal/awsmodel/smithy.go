package awsmodel

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// Smithy JSON AST documents, as published by github.com/aws/api-models-aws.
// Only the parts the loader translates are decoded.
type smithyDocument struct {
	Smithy string                 `json:"smithy"`
	Shapes map[string]smithyShape `json:"shapes"`
}

type smithyShape struct {
	Type    string         `json:"type"`
	Version string         `json:"version"`
	Input   *smithyMember  `json:"input"`
	Output  *smithyMember  `json:"output"`
	Errors  []smithyMember `json:"errors"`
	Members smithyMembers  `json:"members"`
	Member  *smithyMember  `json:"member"`
	Key     *smithyMember  `json:"key"`
	Value   *smithyMember  `json:"value"`
	Traits  smithyTraits   `json:"traits"`
}

type smithyMember struct {
	Target string       `json:"target"`
	Traits smithyTraits `json:"traits"`
}

type namedSmithyMember struct {
	smithyMember

	name string
}

// smithyMembers keeps members in document order, which is the order AWS
// declares enum values and structure members in.
type smithyMembers []namedSmithyMember

func (m *smithyMembers) UnmarshalJSON(data []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	if token, err := decoder.Token(); err != nil || token != json.Delim('{') {
		return fmt.Errorf("members is not an object")
	}
	for decoder.More() {
		token, err := decoder.Token()
		if err != nil {
			return err
		}
		name, ok := token.(string)
		if !ok {
			return fmt.Errorf("member name is not a string")
		}
		var member smithyMember
		if err := decoder.Decode(&member); err != nil {
			return fmt.Errorf("member %q: %w", name, err)
		}
		*m = append(*m, namedSmithyMember{smithyMember: member, name: name})
	}
	_, err := decoder.Token()
	return err
}

type smithyTraits map[string]json.RawMessage

func (t smithyTraits) has(name string) bool {
	_, ok := t[name]
	return ok
}

// decode reports whether the trait is present, and fails if it is present
// but does not decode into value.
func (t smithyTraits) decode(name string, value any) (bool, error) {
	raw, ok := t[name]
	if !ok {
		return false, nil
	}
	if err := json.Unmarshal(raw, value); err != nil {
		return true, fmt.Errorf("trait %s: %w", name, err)
	}
	return true, nil
}

func (t smithyTraits) string(name string) (string, error) {
	var value string
	_, err := t.decode(name, &value)
	return value, err
}

const (
	smithyPrelude = "smithy.api#"
	smithyUnit    = smithyPrelude + "Unit"
)

// smithyProtocols maps a service's protocol trait to the api-2.json protocol
// name the decoders switch on.
var smithyProtocols = map[string]string{
	"aws.protocols#awsJson1_0": "json",
	"aws.protocols#awsJson1_1": "json",
	"aws.protocols#restJson1":  "rest-json",
	"aws.protocols#restXml":    "rest-xml",
	"aws.protocols#awsQuery":   "query",
	"aws.protocols#ec2Query":   "ec2",
}

// smithyTypes maps Smithy shape types onto the api-2.json type vocabulary.
// Enums become constrained strings and unions structures, as they were there.
var smithyTypes = map[string]string{
	"blob":       "blob",
	"boolean":    "boolean",
	"string":     "string",
	"enum":       "string",
	"byte":       "integer",
	"short":      "integer",
	"integer":    "integer",
	"intEnum":    "integer",
	"long":       "long",
	"float":      "float",
	"double":     "double",
	"bigInteger": "long",
	"bigDecimal": "double",
	"timestamp":  "timestamp",
	"document":   "document",
	"list":       "list",
	"map":        "map",
	"structure":  "structure",
	"union":      "structure",
}

// preludeTypes are the Smithy prelude shapes a model may target without
// defining them.
var preludeTypes = map[string]string{
	"Blob":             "blob",
	"Boolean":          "boolean",
	"PrimitiveBoolean": "boolean",
	"String":           "string",
	"Byte":             "integer",
	"PrimitiveByte":    "integer",
	"Short":            "integer",
	"PrimitiveShort":   "integer",
	"Integer":          "integer",
	"PrimitiveInteger": "integer",
	"Long":             "long",
	"PrimitiveLong":    "long",
	"Float":            "float",
	"PrimitiveFloat":   "float",
	"Double":           "double",
	"PrimitiveDouble":  "double",
	"BigInteger":       "long",
	"BigDecimal":       "double",
	"Timestamp":        "timestamp",
	"Document":         "document",
}

// smithyTranslator converts one Smithy service model into the api-2.json
// shaped Model the validators read.
type smithyTranslator struct {
	service  Service
	document smithyDocument
	protocol string
	shapes   map[string]*Shape
}

func parseSmithyModel(service Service, contents []byte) (*Model, error) {
	translator := smithyTranslator{service: service, shapes: map[string]*Shape{}}
	if err := json.Unmarshal(contents, &translator.document); err != nil {
		return nil, fmt.Errorf("awsmodel: parse %s model: %w", service, err)
	}
	if !strings.HasPrefix(translator.document.Smithy, "2.") {
		return nil, fmt.Errorf("awsmodel: %s model is Smithy %q, want 2.x", service, translator.document.Smithy)
	}
	model, err := translator.translate()
	if err != nil {
		return nil, fmt.Errorf("awsmodel: translate %s model: %w", service, err)
	}
	return model, nil
}

func (t *smithyTranslator) translate() (*Model, error) {
	metadata, err := t.metadata()
	if err != nil {
		return nil, err
	}
	t.protocol = metadata.Protocol

	if err := t.checkLocalNames(); err != nil {
		return nil, err
	}

	operations := map[string]*Operation{}
	for id, shape := range t.document.Shapes {
		switch shape.Type {
		case "service", "resource":
			continue
		case "operation":
			operation, err := t.operation(id, shape)
			if err != nil {
				return nil, fmt.Errorf("operation %s: %w", id, err)
			}
			operations[operation.Name] = operation
		default:
			translated, err := t.shape(shape)
			if err != nil {
				return nil, fmt.Errorf("shape %s: %w", id, err)
			}
			t.shapes[localName(id)] = translated
		}
	}
	if len(operations) == 0 || len(t.shapes) == 0 {
		return nil, errors.New("model has no operations or shapes")
	}

	model := &Model{
		service:    t.service,
		metadata:   metadata,
		operations: operations,
		shapes:     t.shapes,
	}
	if err := model.validateReferences(); err != nil {
		return nil, err
	}
	return model, nil
}

func (t *smithyTranslator) metadata() (Metadata, error) {
	var (
		service   *smithyShape
		serviceID string
	)
	for id, shape := range t.document.Shapes {
		if shape.Type != "service" {
			continue
		}
		if service != nil {
			return Metadata{}, fmt.Errorf("model defines more than one service, including %s", id)
		}
		service, serviceID = &shape, id
	}
	if service == nil {
		return Metadata{}, errors.New("model defines no service")
	}

	metadata := Metadata{APIVersion: service.Version}
	for trait, protocol := range smithyProtocols {
		if !service.Traits.has(trait) {
			continue
		}
		if metadata.Protocol != "" {
			return Metadata{}, fmt.Errorf("service declares more than one protocol")
		}
		metadata.Protocol = protocol
		if version, ok := strings.CutPrefix(trait, "aws.protocols#awsJson"); ok {
			metadata.JSONVersion = strings.ReplaceAll(version, "_", ".")
			metadata.TargetPrefix = localName(serviceID)
		}
	}
	if metadata.Protocol == "" {
		return Metadata{}, errors.New("service declares no supported protocol")
	}

	var awsService struct {
		SDKID          string `json:"sdkId"`
		EndpointPrefix string `json:"endpointPrefix"`
	}
	if _, err := service.Traits.decode("aws.api#service", &awsService); err != nil {
		return Metadata{}, err
	}
	title, err := service.Traits.string("smithy.api#title")
	if err != nil {
		return Metadata{}, err
	}
	var sigv4 struct {
		Name string `json:"name"`
	}
	if _, err := service.Traits.decode("aws.auth#sigv4", &sigv4); err != nil {
		return Metadata{}, err
	}
	metadata.EndpointPrefix = awsService.EndpointPrefix
	metadata.ServiceID = awsService.SDKID
	metadata.ServiceFullName = title
	metadata.SigningName = sigv4.Name
	return metadata, nil
}

// checkLocalNames rejects a model whose shapes would collide once their
// namespaces are dropped, which would silently merge two shapes into one.
func (t *smithyTranslator) checkLocalNames() error {
	seen := make(map[string]string, len(t.document.Shapes))
	for id := range t.document.Shapes {
		name := localName(id)
		if other, ok := seen[name]; ok {
			return fmt.Errorf("shapes %s and %s share the local name %q", id, other, name)
		}
		seen[name] = id
	}
	return nil
}

// localName drops the namespace from a model's own shape IDs. Prelude shapes
// keep their absolute ID: a model may define a same-named shape of its own.
func localName(id string) string {
	if strings.HasPrefix(id, smithyPrelude) {
		return id
	}
	if separator := strings.LastIndex(id, "#"); separator >= 0 {
		return id[separator+1:]
	}
	return id
}

func (t *smithyTranslator) operation(id string, shape smithyShape) (*Operation, error) {
	operation := &Operation{Name: localName(id)}

	var httpTrait struct {
		Method string `json:"method"`
		URI    string `json:"uri"`
		Code   int    `json:"code"`
	}
	found, err := shape.Traits.decode("smithy.api#http", &httpTrait)
	if err != nil {
		return nil, err
	}
	if found {
		operation.HTTP = HTTP{Method: httpTrait.Method, RequestURI: httpTrait.URI, ResponseCode: httpTrait.Code}
	} else {
		// The RPC protocols bind every operation to a POST of /.
		operation.HTTP = HTTP{Method: "POST", RequestURI: "/"}
	}

	if operation.Input, err = t.operationRef(shape.Input); err != nil {
		return nil, fmt.Errorf("input: %w", err)
	}
	if operation.Output, err = t.operationRef(shape.Output); err != nil {
		return nil, fmt.Errorf("output: %w", err)
	}
	// awsQuery wraps every modelled output in <OperationResult>, which Smithy
	// leaves to the protocol rather than recording on the operation.
	if operation.Output != nil && t.protocol == "query" {
		operation.Output.ResultWrapper = operation.Name + "Result"
	}
	for _, errorRef := range shape.Errors {
		ref, err := t.ref(errorRef)
		if err != nil {
			return nil, fmt.Errorf("error %s: %w", errorRef.Target, err)
		}
		operation.Errors = append(operation.Errors, ref)
	}
	return operation, nil
}

func (t *smithyTranslator) operationRef(member *smithyMember) (*ShapeRef, error) {
	if member == nil || member.Target == smithyUnit {
		return nil, nil
	}
	ref, err := t.ref(*member)
	if err != nil {
		return nil, err
	}
	return &ref, nil
}

func (t *smithyTranslator) shape(source smithyShape) (*Shape, error) {
	shapeType, ok := smithyTypes[source.Type]
	if !ok {
		return nil, fmt.Errorf("unsupported shape type %q", source.Type)
	}
	shape := &Shape{Type: shapeType}

	switch source.Type {
	case "enum":
		for _, member := range source.Members {
			value, err := member.Traits.string("smithy.api#enumValue")
			if err != nil {
				return nil, fmt.Errorf("enum member %s: %w", member.name, err)
			}
			if value == "" {
				value = member.name
			}
			shape.Enum = append(shape.Enum, value)
		}
	case "structure", "union":
		shape.Union = source.Type == "union"
		shape.Members = make(map[string]ShapeRef, len(source.Members))
		for _, member := range source.Members {
			ref, err := t.memberRef(member)
			if err != nil {
				return nil, fmt.Errorf("member %s: %w", member.name, err)
			}
			shape.Members[member.name] = ref
			if member.Traits.has("smithy.api#required") {
				shape.Required = append(shape.Required, member.name)
			}
			if member.Traits.has("smithy.api#httpPayload") {
				shape.Payload = member.name
			}
		}
	case "list":
		ref, err := t.containerRef("member", source.Member)
		if err != nil {
			return nil, err
		}
		shape.Member = ref
	case "map":
		key, err := t.containerRef("key", source.Key)
		if err != nil {
			return nil, err
		}
		value, err := t.containerRef("value", source.Value)
		if err != nil {
			return nil, err
		}
		shape.Key, shape.Value = key, value
	}

	if err := t.applyShapeTraits(shape, source.Traits); err != nil {
		return nil, err
	}
	return shape, nil
}

func (t *smithyTranslator) applyShapeTraits(shape *Shape, traits smithyTraits) error {
	var bounds struct {
		Min *float64 `json:"min"`
		Max *float64 `json:"max"`
	}
	for _, trait := range []string{"smithy.api#length", "smithy.api#range"} {
		if _, err := traits.decode(trait, &bounds); err != nil {
			return err
		}
	}
	shape.Min, shape.Max = bounds.Min, bounds.Max

	var err error
	if shape.Pattern, err = traits.string("smithy.api#pattern"); err != nil {
		return err
	}
	if shape.LocationName, err = traits.string("smithy.api#xmlName"); err != nil {
		return err
	}
	if shape.TimestampFormat, err = timestampFormat(traits); err != nil {
		return err
	}
	shape.Sensitive = traits.has("smithy.api#sensitive")

	kind, err := traits.string("smithy.api#error")
	if err != nil || kind == "" {
		return err
	}
	shape.Exception = true
	shape.Fault = kind == "server"
	shape.Error = &ErrorInfo{SenderFault: kind == "client"}
	if _, err := traits.decode("smithy.api#httpError", &shape.Error.HTTPStatusCode); err != nil {
		return err
	}
	// A JSON service's awsQueryError is only for awsQueryCompatible clients;
	// its wire code is still the shape name.
	if t.protocol != "query" {
		return nil
	}
	var queryError struct {
		Code             string `json:"code"`
		HTTPResponseCode int    `json:"httpResponseCode"`
	}
	if _, err := traits.decode("aws.protocols#awsQueryError", &queryError); err != nil {
		return err
	}
	shape.Error.Code = queryError.Code
	if queryError.HTTPResponseCode != 0 {
		shape.Error.HTTPStatusCode = queryError.HTTPResponseCode
	}
	return nil
}

// timestampFormats maps Smithy's timestamp formats to the api-2.json names.
var timestampFormats = map[string]string{
	"date-time":     "iso8601",
	"http-date":     "rfc822",
	"epoch-seconds": "unixTimestamp",
}

func timestampFormat(traits smithyTraits) (string, error) {
	format, err := traits.string("smithy.api#timestampFormat")
	if err != nil || format == "" {
		return "", err
	}
	name, ok := timestampFormats[format]
	if !ok {
		return "", fmt.Errorf("unsupported timestamp format %q", format)
	}
	return name, nil
}

func (t *smithyTranslator) containerRef(label string, member *smithyMember) (*ShapeRef, error) {
	if member == nil {
		return nil, fmt.Errorf("%s is missing", label)
	}
	ref, err := t.memberRef(namedSmithyMember{smithyMember: *member, name: label})
	if err != nil {
		return nil, fmt.Errorf("%s: %w", label, err)
	}
	return &ref, nil
}

// memberRef translates a member's wire-binding traits onto the reference.
func (t *smithyTranslator) memberRef(member namedSmithyMember) (ShapeRef, error) {
	ref, err := t.ref(member.smithyMember)
	if err != nil {
		return ShapeRef{}, err
	}
	traits := member.Traits

	names := []struct {
		trait string
		value *string
	}{
		{"smithy.api#xmlName", &ref.LocationName},
		{"smithy.api#jsonName", &ref.LocationName},
		{"aws.protocols#ec2QueryName", &ref.QueryName},
	}
	for _, binding := range names {
		if value, err := traits.string(binding.trait); err != nil {
			return ShapeRef{}, err
		} else if value != "" {
			*binding.value = value
		}
	}
	if ref.TimestampFormat, err = timestampFormat(traits); err != nil {
		return ShapeRef{}, err
	}
	ref.Flattened = traits.has("smithy.api#xmlFlattened")
	ref.XMLAttribute = traits.has("smithy.api#xmlAttribute")

	httpBindings := []struct {
		trait    string
		location string
	}{
		{"smithy.api#httpHeader", "header"},
		{"smithy.api#httpPrefixHeaders", "headers"},
		{"smithy.api#httpQuery", "querystring"},
	}
	for _, binding := range httpBindings {
		if !traits.has(binding.trait) {
			continue
		}
		name, err := traits.string(binding.trait)
		if err != nil {
			return ShapeRef{}, err
		}
		ref.Location, ref.LocationName = binding.location, name
	}
	switch {
	case traits.has("smithy.api#httpLabel"):
		ref.Location, ref.LocationName = "uri", member.name
	case traits.has("smithy.api#httpQueryParams"):
		ref.Location = "querystring"
	case traits.has("smithy.api#httpResponseCode"):
		ref.Location = "statusCode"
	}
	return ref, nil
}

// ref resolves a target to its local name, registering any prelude shape it
// names so reference validation can find it.
func (t *smithyTranslator) ref(member smithyMember) (ShapeRef, error) {
	target := member.Target
	if name, isPrelude := strings.CutPrefix(target, smithyPrelude); isPrelude {
		preludeType, ok := preludeTypes[name]
		if !ok {
			return ShapeRef{}, fmt.Errorf("unsupported prelude target %s", target)
		}
		t.shapes[target] = &Shape{Type: preludeType}
		return ShapeRef{Shape: target}, nil
	}
	shape, ok := t.document.Shapes[target]
	if !ok {
		return ShapeRef{}, fmt.Errorf("unknown target %s", target)
	}
	return ShapeRef{Shape: localName(target), Streaming: shape.Traits.has("smithy.api#streaming")}, nil
}
