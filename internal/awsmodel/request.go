package awsmodel

import (
	"fmt"
	"maps"
	"math"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
)

// Constraint names the modelled constraint a rejection request breaks.
type Constraint string

const (
	ConstraintRequired Constraint = "required"
	ConstraintEnum     Constraint = "enum"
	ConstraintLength   Constraint = "length"
	ConstraintRange    Constraint = "range"
	ConstraintPattern  Constraint = "pattern"
)

// RequestCase is one generated request. Every case carries the operation's
// required members plus at most one optional top-level member, named by
// Member, so a rejection points at the member that caused it. An acceptance
// case has no Constraint; a rejection case breaks exactly the constraint it
// names, at Path.
type RequestCase struct {
	Operation  string
	Member     string
	Constraint Constraint
	Path       string
	Detail     string
	Input      map[string]any
}

// Acceptance reports whether the case should be accepted.
func (c RequestCase) Acceptance() bool { return c.Constraint == "" }

// RequestPlan is every request generated for one operation, acceptance cases
// first. Skipped lists the members left out of the requests and the
// constraints that were not broken, each with the reason.
type RequestPlan struct {
	Cases   []RequestCase
	Skipped []SkippedMember
}

// SkippedMember names a member the generator could not exercise.
type SkippedMember struct {
	Path   string
	Reason string
}

// GenerateRequests builds, for an operation's input, an acceptance request
// with only the required members, one adding each optional top-level member,
// and one rejection request per breakable constraint. Values are valid by the
// model and of the right form, but identifiers name nothing that exists.
func GenerateRequests(service Service, operationName string, options RequestOptions) (RequestPlan, error) {
	model, err := Load(service)
	if err != nil {
		return RequestPlan{}, err
	}
	operation, ok := model.Operation(operationName)
	if !ok {
		return RequestPlan{}, fmt.Errorf("awsmodel: %s operation %q is not modelled", service, operationName)
	}
	if operation.Input == nil {
		return RequestPlan{Cases: []RequestCase{{Operation: operationName, Input: map[string]any{}}}}, nil
	}

	first := model.newRequestGenerator(0, options)
	full, ok := first.structure(operation.Input.Shape, nil, 0)
	plan := RequestPlan{Skipped: first.skipped}
	if !ok {
		plan.Skipped = append(plan.Skipped, SkippedMember{Path: "$", Reason: "a required member cannot be generated"})
		return plan, nil
	}
	input := model.shapes[operation.Input.Shape]
	optional := make([]string, 0, len(full))
	for _, member := range slices.Sorted(maps.Keys(full)) {
		if !slices.Contains(input.Required, member) {
			optional = append(optional, member)
		}
	}

	// Each case is generated afresh with its own salt, so the names a create
	// request carries differ between cases and one case cannot collide with
	// what an earlier case created.
	salt := 0
	generate := func() (map[string]any, []constraintSite, error) {
		generator := model.newRequestGenerator(salt, options)
		salt++
		value, _ := generator.structure(operation.Input.Shape, nil, 0)
		if len(generator.sites) != len(first.sites) {
			return nil, nil, fmt.Errorf("awsmodel: %s %s generated different constraints for different salts", service, operationName)
		}
		return value, generator.sites, nil
	}

	for _, member := range append([]string{""}, optional...) {
		if unguessable[member] {
			continue
		}
		value, _, err := generate()
		if err != nil {
			return RequestPlan{}, err
		}
		plan.Cases = append(plan.Cases, RequestCase{
			Operation: operationName,
			Member:    member,
			Path:      memberPathOf(member),
			Input:     keepMembers(value, input.Required, member),
		})
	}
	for index := range first.sites {
		value, sites, err := generate()
		if err != nil {
			return RequestPlan{}, err
		}
		site := sites[index]
		member, _ := site.path[0].(string)
		if slices.Contains(input.Required, member) {
			member = ""
		}
		broken, err := site.apply(keepMembers(value, input.Required, member))
		if err != nil {
			return RequestPlan{}, err
		}
		plan.Cases = append(plan.Cases, RequestCase{
			Operation:  operationName,
			Member:     member,
			Constraint: site.constraint,
			Path:       formatPath(site.path),
			Detail:     site.detail,
			Input:      broken,
		})
	}
	return plan, nil
}

// unguessable members take values the model cannot describe: a pagination
// token the service issued, or a filter name only its documentation lists.
// They get rejection requests but no acceptance request.
var unguessable = map[string]bool{
	"Filter":    true,
	"Filters":   true,
	"Marker":    true,
	"NextToken": true,
	"nextToken": true,
}

func memberPathOf(member string) string {
	if member == "" {
		return "$"
	}
	return "$." + member
}

// keepMembers drops every top-level member but the required ones and member.
func keepMembers(input map[string]any, required []string, member string) map[string]any {
	for name := range input {
		if name != member && !slices.Contains(required, name) {
			delete(input, name)
		}
	}
	return input
}

// pathStep is a structure member or map key (string) or a list index (int).
type pathStep any

func formatPath(path []pathStep) string {
	var builder strings.Builder
	builder.WriteString("$")
	for _, step := range path {
		switch step := step.(type) {
		case int:
			fmt.Fprintf(&builder, "[%d]", step)
		case string:
			builder.WriteString(".")
			builder.WriteString(step)
		}
	}
	return builder.String()
}

// constraintSite is one constraint a rejection request can break: it either
// removes a required member or replaces the value at path.
type constraintSite struct {
	path        []pathStep
	constraint  Constraint
	detail      string
	remove      bool
	replacement any
	// build makes a replacement too costly to make for every case.
	build func() any
}

// apply returns input with the site's constraint broken. input is a freshly
// generated value tree that nothing else holds.
func (s constraintSite) apply(input map[string]any) (map[string]any, error) {
	var container any = input
	for _, step := range s.path[:len(s.path)-1] {
		switch step := step.(type) {
		case int:
			list, ok := container.([]any)
			if !ok {
				return nil, fmt.Errorf("awsmodel: %s: want a list", formatPath(s.path))
			}
			container = list[step]
		case string:
			fields, ok := container.(map[string]any)
			if !ok {
				return nil, fmt.Errorf("awsmodel: %s: want a structure or map", formatPath(s.path))
			}
			container = fields[step]
		}
	}
	replacement := s.replacement
	if s.build != nil {
		replacement = s.build()
	}
	switch last := s.path[len(s.path)-1].(type) {
	case int:
		list, ok := container.([]any)
		if !ok {
			return nil, fmt.Errorf("awsmodel: %s: want a list", formatPath(s.path))
		}
		list[last] = replacement
	case string:
		fields, ok := container.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("awsmodel: %s: want a structure or map", formatPath(s.path))
		}
		if s.remove {
			delete(fields, last)
		} else {
			fields[last] = replacement
		}
	}
	return input, nil
}

// maxStructureDepth stops recursive shapes, such as nested filters.
const maxStructureDepth = 8

// maxListRejectionLength keeps a request that breaks a list maximum small.
const maxListRejectionLength = 1000

var generatedTimestamp = time.Date(2026, time.January, 1, 0, 0, 0, 0, time.UTC)

type requestGenerator struct {
	model      *Model
	options    RequestOptions
	salt       string
	saltNumber int
	sites      []constraintSite
	skipped    []SkippedMember
	visiting   map[string]bool
}

func (m *Model) newRequestGenerator(salt int, options RequestOptions) *requestGenerator {
	return &requestGenerator{model: m, options: options, salt: saltLetters(salt), saltNumber: salt, visiting: map[string]bool{}}
}

// saltLetters spells n in base 26 with lower-case letters, which fit more
// patterns than digits do.
func saltLetters(n int) string {
	letters := ""
	for {
		letters = string(rune('a'+n%26)) + letters
		n /= 26
		if n == 0 {
			return letters
		}
	}
}

func (g *requestGenerator) skip(path []pathStep, reason string) {
	g.skipped = append(g.skipped, SkippedMember{Path: formatPath(path), Reason: reason})
}

func (g *requestGenerator) site(path []pathStep, site constraintSite) {
	site.path = slices.Clone(path)
	g.sites = append(g.sites, site)
}

// value generates a valid value for ref at path, reporting false when none
// can be generated.
func (g *requestGenerator) value(ref ShapeRef, path []pathStep, depth int) (any, bool) {
	shape := g.model.shapes[ref.Shape]
	if ref.Streaming {
		g.skip(path, "streaming payload")
		return nil, false
	}
	switch shape.Type {
	case "structure":
		if value, ok := g.structure(ref.Shape, path, depth+1); ok {
			return value, true
		}
		return nil, false
	case "list":
		return g.list(shape, path, depth)
	case "map":
		return g.mapValue(shape, path, depth)
	case "string":
		return g.stringValue(shape, ref, path)
	case "integer", "long":
		return g.integer(shape, path), true
	case "float", "double":
		return g.float(shape, path), true
	case "boolean":
		return false, true
	case "timestamp":
		return generatedTimestamp, true
	case "blob":
		return g.blob(shape, path), true
	default:
		g.skip(path, shape.Type+" values are not generated")
		return nil, false
	}
}

func (g *requestGenerator) structure(name string, path []pathStep, depth int) (map[string]any, bool) {
	if depth > maxStructureDepth || g.visiting[name] {
		g.skip(path, "recursive structure")
		return nil, false
	}
	g.visiting[name] = true
	defer delete(g.visiting, name)

	shape := g.model.shapes[name]
	required := make(map[string]bool, len(shape.Required))
	for _, member := range shape.Required {
		required[member] = true
	}
	value := map[string]any{}
	for _, member := range slices.Sorted(maps.Keys(shape.Members)) {
		ref := shape.Members[member]
		memberPath := append(slices.Clone(path), member)
		memberValue, ok := g.value(ref, memberPath, depth)
		if !ok {
			if required[member] {
				return nil, false
			}
			continue
		}
		value[member] = memberValue
		// An empty URI label changes which route the request reaches, rather
		// than omitting a member from it.
		if required[member] && ref.Location != "uri" {
			g.site(memberPath, constraintSite{constraint: ConstraintRequired, detail: "member omitted", remove: true})
		}
		// A union takes exactly one member.
		if shape.Union {
			break
		}
	}
	return value, true
}

func (g *requestGenerator) list(shape *Shape, path []pathStep, depth int) (any, bool) {
	element, ok := g.value(*shape.Member, append(slices.Clone(path), 0), depth)
	if !ok {
		return nil, false
	}
	count := 1
	if shape.Min != nil {
		count = max(count, int(*shape.Min))
	}
	if shape.Max != nil && count > int(*shape.Max) {
		return nil, false
	}
	list := make([]any, count)
	for i := range list {
		list[i] = element
		if i > 0 {
			list[i], _ = g.quietValue(*shape.Member, depth)
		}
	}

	if shape.Min != nil && *shape.Min > 0 {
		short := slices.Clone(list[:int(*shape.Min)-1])
		g.site(path, constraintSite{constraint: ConstraintLength, detail: fmt.Sprintf("%d items, below min %g", len(short), *shape.Min), replacement: short})
	}
	if shape.Max != nil && *shape.Max < maxListRejectionLength {
		build := func() any {
			long := make([]any, int(*shape.Max)+1)
			for i := range long {
				long[i], _ = g.quietValue(*shape.Member, depth)
			}
			return long
		}
		g.site(path, constraintSite{constraint: ConstraintLength, detail: fmt.Sprintf("%d items, above max %g", int(*shape.Max)+1, *shape.Max), build: build})
	}
	return list, true
}

// quietValue generates a further valid value without recording sites or
// skips, for list entries beyond the first.
func (g *requestGenerator) quietValue(ref ShapeRef, depth int) (any, bool) {
	sites, skipped := len(g.sites), len(g.skipped)
	value, ok := g.value(ref, nil, depth)
	g.sites, g.skipped = g.sites[:sites], g.skipped[:skipped]
	return value, ok
}

func (g *requestGenerator) mapValue(shape *Shape, path []pathStep, depth int) (any, bool) {
	key, ok := g.quietValue(*shape.Key, depth)
	if !ok {
		g.skip(path, "map key cannot be generated")
		return nil, false
	}
	keyString, ok := key.(string)
	if !ok {
		g.skip(path, "map key is not a string")
		return nil, false
	}
	value, ok := g.value(*shape.Value, append(slices.Clone(path), keyString), depth)
	if !ok {
		return nil, false
	}
	return map[string]any{keyString: value}, true
}

func (g *requestGenerator) stringValue(shape *Shape, ref ShapeRef, path []pathStep) (any, bool) {
	if len(shape.Enum) > 0 {
		g.site(path, constraintSite{constraint: ConstraintEnum, detail: "value not in enum", replacement: invalidEnumValue(shape.Enum)})
		return shape.Enum[0], true
	}

	minLength, maxLength := 0, maxGeneratedLength
	if shape.Min != nil {
		minLength = int(*shape.Min)
	}
	if shape.Max != nil {
		maxLength = min(maxLength, int(*shape.Max))
	}

	var sampler *patternSampler
	if shape.Pattern != "" {
		var err error
		if sampler, err = newPatternSampler(shape.Pattern); err != nil {
			g.skip(path, "pattern is not supported by Go regexp")
			return nil, false
		}
	}
	value, ok := g.hint(path)
	if !ok || !fits(value, sampler, minLength, maxLength) {
		value, ok = g.validString(sampler, minLength, maxLength)
	}
	if !ok {
		g.skip(path, "no value satisfies the pattern and length")
		return nil, false
	}

	label := ref.Location == "uri"
	if shape.Min != nil && minLength > 0 && (!label || minLength != 1) {
		g.site(path, g.lengthSite(sampler, 0, minLength-1, minLength-1, fmt.Sprintf("below min %d", minLength)))
	}
	if shape.Max != nil && maxLength < maxGeneratedLength {
		g.site(path, g.lengthSite(sampler, maxLength+1, maxGeneratedLength, maxLength+1, fmt.Sprintf("above max %d", maxLength)))
	}
	if sampler != nil {
		if broken, ok := patternBreaker(sampler.re, max(minLength, 1), maxLength); ok {
			g.site(path, constraintSite{constraint: ConstraintPattern, detail: "value does not match " + shape.Pattern, replacement: broken})
		} else {
			g.skip(path, "no value of a valid length breaks the pattern")
		}
	}
	return value, true
}

func fits(value string, sampler *patternSampler, minLength, maxLength int) bool {
	length := len([]rune(value))
	return length >= minLength && length <= maxLength && (sampler == nil || sampler.fullMatch(value))
}

// validString builds a salted string that satisfies the pattern and length.
func (g *requestGenerator) validString(sampler *patternSampler, minLength, maxLength int) (string, bool) {
	if sampler == nil {
		return fitLength("spx"+g.salt, minLength, maxLength), minLength <= maxLength
	}
	base, ok := sampler.sample(minLength, maxLength)
	if !ok {
		return "", false
	}
	for _, candidate := range []string{base + g.salt, g.salt + base} {
		if fits(candidate, sampler, minLength, maxLength) {
			return candidate, true
		}
	}
	return base, true
}

// lengthSite breaks a string length, keeping to the pattern where it can and
// otherwise falling back to a plain string of fallback runes.
func (g *requestGenerator) lengthSite(sampler *patternSampler, minLength, maxLength, fallback int, detail string) constraintSite {
	site := constraintSite{constraint: ConstraintLength, detail: detail}
	if sampler != nil {
		if value, ok := sampler.sample(minLength, maxLength); ok {
			site.replacement = value
			return site
		}
		site.detail += ", also breaking the pattern"
	}
	site.replacement = fitLength("spx"+g.salt, fallback, fallback)
	return site
}

// fitLength pads value with 'a' to minLength runes and truncates it to maxLength.
func fitLength(value string, minLength, maxLength int) string {
	runes := []rune(value)
	for len(runes) < minLength {
		runes = append(runes, 'a')
	}
	if len(runes) > maxLength {
		runes = runes[:max(maxLength, 0)]
	}
	return string(runes)
}

// patternBreaker finds a string of a valid length that does not match re.
func patternBreaker(re *regexp.Regexp, minLength, maxLength int) (string, bool) {
	length := min(max(minLength, 1), maxLength)
	for _, filler := range []string{"~", "!", " ", "#", "-", "Z", "0", "a"} {
		candidate := strings.Repeat(filler, length)
		if !re.MatchString(candidate) {
			return candidate, true
		}
	}
	return "", false
}

func invalidEnumValue(values []string) string {
	candidate := "NotAModelledValue"
	for slices.Contains(values, candidate) {
		candidate += "X"
	}
	return candidate
}

func (g *requestGenerator) integer(shape *Shape, path []pathStep) int64 {
	value := int64(1)
	if shape.Min != nil {
		value = max(value, int64(math.Ceil(*shape.Min)))
	}
	if shape.Max != nil {
		value = min(value, int64(math.Floor(*shape.Max)))
	}
	if shape.Min != nil && *shape.Min > math.MinInt64+1 {
		below := int64(math.Ceil(*shape.Min)) - 1
		g.site(path, constraintSite{constraint: ConstraintRange, detail: "below min " + strconv.FormatFloat(*shape.Min, 'g', -1, 64), replacement: below})
	}
	if shape.Max != nil && *shape.Max < math.MaxInt64-1 {
		above := int64(math.Floor(*shape.Max)) + 1
		g.site(path, constraintSite{constraint: ConstraintRange, detail: "above max " + strconv.FormatFloat(*shape.Max, 'g', -1, 64), replacement: above})
	}
	return value
}

func (g *requestGenerator) float(shape *Shape, path []pathStep) float64 {
	value := 1.0
	if shape.Min != nil {
		value = max(value, *shape.Min)
		g.site(path, constraintSite{constraint: ConstraintRange, detail: "below min " + strconv.FormatFloat(*shape.Min, 'g', -1, 64), replacement: *shape.Min - 1})
	}
	if shape.Max != nil {
		value = min(value, *shape.Max)
		g.site(path, constraintSite{constraint: ConstraintRange, detail: "above max " + strconv.FormatFloat(*shape.Max, 'g', -1, 64), replacement: *shape.Max + 1})
	}
	return value
}

func (g *requestGenerator) blob(shape *Shape, path []pathStep) []byte {
	minLength, maxLength := 0, maxGeneratedLength
	if shape.Min != nil {
		minLength = int(*shape.Min)
		if minLength > 0 {
			g.site(path, constraintSite{constraint: ConstraintLength, detail: fmt.Sprintf("below min %d bytes", minLength), replacement: []byte(fitLength("", minLength-1, minLength-1))})
		}
	}
	if shape.Max != nil {
		maxLength = min(maxLength, int(*shape.Max))
		if maxLength < maxGeneratedLength {
			g.site(path, constraintSite{constraint: ConstraintLength, detail: fmt.Sprintf("above max %d bytes", maxLength), replacement: []byte(fitLength("", maxLength+1, maxLength+1))})
		}
	}
	return []byte(fitLength("spx"+g.salt, minLength, maxLength))
}
