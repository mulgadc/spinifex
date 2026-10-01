package bodyscope_test

import (
	"encoding/json"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/mulgadc/spinifex/spinifex/gateway/bodyscope"
)

// A lookup is one identifier an authz resolver reads through bodyscope, paired
// with the field shape the handler decodes the same body into. Every handler
// uses encoding/json, whose field matching depends only on name and type, so a
// one-field struct stands in for each action's request type.
type lookup struct {
	path []string // Object hops, then the String/Strings field
	list bool
	sdk  bool // aws-sdk-go shape: untagged pointer fields; else tagged values
}

// Mirrors the String/Strings/Object calls in the ecrapi, ecs, eks, acm and
// bedrock resolvers. ECR's own request structs are tagged value types.
var lookups = []lookup{
	{path: []string{"repositoryName"}},
	{path: []string{"repositoryNames"}, list: true},
	{path: []string{"resourceArn"}},
	{path: []string{"accountId"}, sdk: true},
	{path: []string{"capacityProvider"}, sdk: true},
	{path: []string{"capacityProviders"}, list: true, sdk: true},
	{path: []string{"certificateArn"}, sdk: true},
	{path: []string{"cluster"}, sdk: true},
	{path: []string{"clusterName"}, sdk: true},
	{path: []string{"clusters"}, list: true, sdk: true},
	{path: []string{"containerInstance"}, sdk: true},
	{path: []string{"containerInstances"}, list: true, sdk: true},
	{path: []string{"modelId"}, sdk: true},
	{path: []string{"name"}, sdk: true},
	{path: []string{"resourceArn"}, sdk: true},
	{path: []string{"service"}, sdk: true},
	{path: []string{"serviceName"}, sdk: true},
	{path: []string{"services"}, list: true, sdk: true},
	{path: []string{"task"}, sdk: true},
	{path: []string{"taskDefinition"}, sdk: true},
	{path: []string{"tasks"}, list: true, sdk: true},
	{path: []string{"retrieveAndGenerateConfiguration", "knowledgeBaseConfiguration", "knowledgeBaseId"}, sdk: true},
}

// structFor builds the handler-side type for path: a chain of one-field
// structs ending in the identifier field.
func structFor(l lookup, path []string) reflect.Type {
	name := path[0]
	var leaf reflect.Type
	switch {
	case len(path) > 1:
		leaf = structFor(l, path[1:])
	case l.list && l.sdk:
		leaf = reflect.TypeFor[[]*string]()
	case l.list:
		leaf = reflect.TypeFor[[]string]()
	case l.sdk:
		leaf = reflect.TypeFor[*string]()
	default:
		leaf = reflect.TypeFor[string]()
	}
	field := reflect.StructField{Name: strings.ToUpper(name[:1]) + name[1:], Type: leaf}
	if l.sdk {
		if len(path) > 1 {
			field.Type = reflect.PointerTo(leaf)
		}
	} else {
		field.Tag = reflect.StructTag(`json:"` + name + `"`)
	}
	return reflect.StructOf([]reflect.StructField{field})
}

// handlerValues decodes body the way the handler does and returns the
// identifier it would act on, with empty and null list entries dropped to
// match Strings. ok is false when the handler would reject the body.
func handlerValues(l lookup, body []byte) (values []string, ok bool) {
	v := reflect.New(structFor(l, l.path))
	if err := json.Unmarshal(body, v.Interface()); err != nil {
		return nil, false
	}
	cur := v.Elem().Field(0)
	for cur.Kind() == reflect.Pointer || cur.Kind() == reflect.Struct {
		if cur.Kind() == reflect.Pointer {
			if cur.IsNil() {
				return nil, true
			}
			cur = cur.Elem()
			continue
		}
		cur = cur.Field(0)
	}
	if cur.Kind() == reflect.String {
		if s := cur.String(); s != "" {
			return []string{s}, true
		}
		return nil, true
	}
	for i := range cur.Len() {
		e := cur.Index(i)
		if e.Kind() == reflect.Pointer {
			if e.IsNil() {
				continue
			}
			e = e.Elem()
		}
		if s := e.String(); s != "" {
			values = append(values, s)
		}
	}
	return values, true
}

// gateValues resolves the identifier the way the authz resolvers do. ok is
// false when bodyscope refuses the body, which fails the request closed.
func gateValues(l lookup, body []byte) (values []string, ok bool) {
	scope, err := bodyscope.Parse("Fuzz", body)
	if err != nil {
		return nil, false
	}
	for _, hop := range l.path[:len(l.path)-1] {
		if scope, err = scope.Object(hop); err != nil {
			return nil, false
		}
	}
	field := l.path[len(l.path)-1]
	if l.list {
		return scope.Strings(field), true
	}
	if s := scope.String(field); s != "" {
		return []string{s}, true
	}
	return nil, true
}

// FuzzGateMatchesHandler pins the property the authz gate rests on: for any
// body both sides accept, the identifier bodyscope resolves is the one the
// handler decodes. A mismatch authorizes one resource and acts on another.
func FuzzGateMatchesHandler(f *testing.F) {
	for _, seed := range []string{
		`{"repositoryName":"a"}`,
		`{"RepositoryName":"a"}`,
		`{"repositoryNames":["a","","b"]}`,
		`{"cluster":"a","clusters":["b",null]}`,
		`{"retrieveAndGenerateConfiguration":{"knowledgeBaseConfiguration":{"knowledgeBaseId":"kb"}}}`,
		// U+017F folds to "s" in encoding/json but not in strings.ToLower.
		`{"repositoryName":"allowed","repoſitoryName":"victim"}`,
		`{"cluſter":"victim"}`,
		// A repeated key set to null clears a map entry but not a value field.
		`{"repositoryName":"victim","repositoryName":null}`,
		// A repeated object replaces a map entry but merges into a struct.
		`{"retrieveAndGenerateConfiguration":{"knowledgeBaseConfiguration":{"knowledgeBaseId":"victim"}},"retrieveAndGenerateConfiguration":{"type":"KNOWLEDGE_BASE"}}`,
	} {
		f.Add([]byte(seed))
	}

	f.Fuzz(func(t *testing.T, body []byte) {
		for _, l := range lookups {
			gate, gateOK := gateValues(l, body)
			handler, handlerOK := handlerValues(l, body)
			if !gateOK || !handlerOK {
				continue
			}
			if !slices.Equal(gate, handler) {
				t.Errorf("%s (sdk=%v): gate resolved %q, handler decoded %q from %q",
					strings.Join(l.path, "."), l.sdk, gate, handler, body)
			}
		}
	})
}
