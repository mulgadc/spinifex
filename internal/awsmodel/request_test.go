package awsmodel

//test:in-package — checks generated inputs against the unexported model
//shapes, and the unexported pattern sampler and salt helpers.

import (
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/stretchr/testify/require"
)

var testRequestOptions = RequestOptions{AccountID: "123456789012", Region: "ap-southeast-2"}

// constraintViolation is one model constraint an input breaks, found by
// walking the input against the model independently of the generator.
type constraintViolation struct {
	path       string
	constraint Constraint
}

func (m *Model) inputViolations(shapeName, path string, value any, violations *[]constraintViolation) {
	shape := m.shapes[shapeName]
	add := func(constraint Constraint) {
		*violations = append(*violations, constraintViolation{path: path, constraint: constraint})
	}
	outside := func(length float64) bool {
		return (shape.Min != nil && length < *shape.Min) || (shape.Max != nil && length > *shape.Max)
	}
	switch shape.Type {
	case "structure":
		fields := value.(map[string]any)
		for _, member := range shape.Required {
			if _, ok := fields[member]; !ok {
				*violations = append(*violations, constraintViolation{path: path + "." + member, constraint: ConstraintRequired})
			}
		}
		for member, field := range fields {
			m.inputViolations(shape.Members[member].Shape, path+"."+member, field, violations)
		}
	case "list":
		items := value.([]any)
		if outside(float64(len(items))) {
			add(ConstraintLength)
		}
		for i, item := range items {
			m.inputViolations(shape.Member.Shape, fmt.Sprintf("%s[%d]", path, i), item, violations)
		}
	case "map":
		for key, entry := range value.(map[string]any) {
			m.inputViolations(shape.Key.Shape, path+"."+key, key, violations)
			m.inputViolations(shape.Value.Shape, path+"."+key, entry, violations)
		}
	case "string":
		text := value.(string)
		if len(shape.Enum) > 0 {
			if !slices.Contains(shape.Enum, text) {
				add(ConstraintEnum)
			}
			return
		}
		if outside(float64(utf8.RuneCountInString(text))) {
			add(ConstraintLength)
		}
		if shape.Pattern != "" {
			if !testPattern(shape.Pattern).MatchString(text) {
				add(ConstraintPattern)
			}
		}
	case "integer", "long":
		if outside(float64(value.(int64))) {
			add(ConstraintRange)
		}
	case "float", "double":
		if outside(value.(float64)) {
			add(ConstraintRange)
		}
	case "blob":
		if outside(float64(len(value.([]byte)))) {
			add(ConstraintLength)
		}
	case "boolean":
		_ = value.(bool)
	case "timestamp":
		_ = value.(time.Time)
	}
}

var testPatterns sync.Map

func testPattern(pattern string) *regexp.Regexp {
	if re, ok := testPatterns.Load(pattern); ok {
		return re.(*regexp.Regexp)
	}
	re := regexp.MustCompile(javaEscape.ReplaceAllString(pattern, `\x{$1}`))
	testPatterns.Store(pattern, re)
	return re
}

// TestGeneratedRequestsBreakOnlyTheirConstraint checks every modelled
// operation: an acceptance request breaks no constraint, and a rejection
// request breaks its named constraint at its path and nothing elsewhere.
func TestGeneratedRequestsBreakOnlyTheirConstraint(t *testing.T) {
	t.Parallel()
	if testing.Short() {
		t.Skip("generates requests for every modelled operation")
	}
	for _, service := range Services() {
		model, err := Load(service)
		require.NoError(t, err)
		t.Run(string(service), func(t *testing.T) {
			t.Parallel()
			for _, operationName := range model.Operations() {
				operation, _ := model.Operation(operationName)
				if operation.Input == nil {
					continue
				}
				plan, err := GenerateRequests(service, operationName, testRequestOptions)
				require.NoError(t, err, operationName)
				input := model.shapes[operation.Input.Shape]
				for _, request := range plan.Cases {
					var violations []constraintViolation
					model.inputViolations(operation.Input.Shape, "$", request.Input, &violations)
					for member := range request.Input {
						if !slices.Contains(input.Required, member) {
							require.Equal(t, request.Member, member, "%s %s %s carries an unrelated optional member", operationName, request.Constraint, request.Path)
						}
					}
					if request.Acceptance() {
						require.Empty(t, violations, "%s acceptance %s", operationName, request.Path)
						continue
					}
					require.NotEmpty(t, violations, "%s %s %s breaks nothing", operationName, request.Constraint, request.Path)
					for _, violation := range violations {
						require.Equal(t, request.Path, violation.path, "%s %s %s also breaks %v", operationName, request.Constraint, request.Path, violation)
					}
					require.Contains(t, violations, constraintViolation{path: request.Path, constraint: request.Constraint}, operationName)
				}
			}
		})
	}
}

func TestGenerateRequestsIsolatesEachOptionalMember(t *testing.T) {
	plan, err := GenerateRequests(IAM, "CreateRole", testRequestOptions)
	require.NoError(t, err)

	acceptance := map[string]map[string]any{}
	for _, request := range plan.Cases {
		if request.Acceptance() {
			acceptance[request.Member] = request.Input
		}
	}
	require.ElementsMatch(t, []string{"AssumeRolePolicyDocument", "RoleName"}, slices.Collect(maps.Keys(acceptance[""])))
	require.ElementsMatch(t, []string{"AssumeRolePolicyDocument", "RoleName", "Tags"}, slices.Collect(maps.Keys(acceptance["Tags"])))
	require.ElementsMatch(t, []string{"", "Description", "MaxSessionDuration", "Path", "PermissionsBoundary", "Tags"}, slices.Collect(maps.Keys(acceptance)))

	// Every case gets its own names, so one case cannot collide with what an
	// earlier case created.
	names := map[any]bool{}
	for _, request := range plan.Cases {
		if name, ok := request.Input["RoleName"]; ok && request.Path != "$.RoleName" {
			require.False(t, names[name], "RoleName %v reused", name)
			names[name] = true
		}
	}
}

func TestGenerateRequestsAttributesRejections(t *testing.T) {
	plan, err := GenerateRequests(IAM, "CreateRole", testRequestOptions)
	require.NoError(t, err)

	byPath := map[string]RequestCase{}
	for _, request := range plan.Cases {
		if !request.Acceptance() {
			byPath[string(request.Constraint)+" "+request.Path] = request
		}
	}
	// A broken required member is judged against the required-only request;
	// a broken optional one against the request that adds just that member.
	require.Empty(t, byPath["required $.RoleName"].Member)
	require.Equal(t, "MaxSessionDuration", byPath["range $.MaxSessionDuration"].Member)
	require.Equal(t, "Tags", byPath["required $.Tags[0].Key"].Member)
}

func TestGenerateRequestsSkipsAcceptanceForUnguessableMembers(t *testing.T) {
	plan, err := GenerateRequests(IAM, "ListRoles", testRequestOptions)
	require.NoError(t, err)
	var acceptance, markerRejections []string
	for _, request := range plan.Cases {
		if request.Acceptance() {
			acceptance = append(acceptance, request.Member)
		} else if request.Member == "Marker" {
			markerRejections = append(markerRejections, string(request.Constraint))
		}
	}
	require.ElementsMatch(t, []string{"", "MaxItems", "PathPrefix"}, acceptance)
	require.NotEmpty(t, markerRejections, "a pagination token still gets rejection requests")
}

func TestGenerateRequestsUsesResourceHints(t *testing.T) {
	tests := []struct {
		service   Service
		operation string
		member    string
		want      *regexp.Regexp
	}{
		{EC2, "TerminateInstances", "InstanceIds", regexp.MustCompile(`^i-[0-9a-f]{17}$`)},
		{EC2, "DeleteSecurityGroup", "GroupId", regexp.MustCompile(`^sg-[0-9a-f]{17}$`)},
		{ElasticLoadBalancingV2, "DeleteLoadBalancer", "LoadBalancerArn", regexp.MustCompile(`^arn:aws:elasticloadbalancing:ap-southeast-2:123456789012:loadbalancer/app/`)},
		{IAM, "AttachRolePolicy", "PolicyArn", regexp.MustCompile(`^arn:aws:iam::123456789012:policy/`)},
		{RDS, "CreateDBInstance", "Engine", regexp.MustCompile(`^postgres$`)},
	}
	for _, test := range tests {
		t.Run(test.operation+"."+test.member, func(t *testing.T) {
			plan, err := GenerateRequests(test.service, test.operation, testRequestOptions)
			require.NoError(t, err)
			for _, request := range plan.Cases {
				if !request.Acceptance() {
					continue
				}
				value, ok := request.Input[test.member]
				if !ok {
					continue
				}
				if values, isList := value.([]any); isList {
					value = values[0]
				}
				require.Regexp(t, test.want, value)
				return
			}
			t.Fatalf("no acceptance request sets %s", test.member)
		})
	}
}

func TestPatternSamplerMatchesWholeValue(t *testing.T) {
	tests := []struct {
		pattern  string
		min, max int
	}{
		{`^[\w+=,.@-]+$`, 1, 64},
		{`(/)|(/[!-~]+/)`, 1, 512},
		{`[a-z]{3}-[0-9]{4}`, 8, 8},
		{`^arn:aws:iam::\d{12}:role/[a-zA-Z0-9]+$`, 20, 2048},
		{`(Tcp|Udp)`, 3, 3},
		{`[\p{L}\p{Z}\p{N}_.:/=+\-@]*`, 0, 128},
	}
	for _, test := range tests {
		t.Run(test.pattern, func(t *testing.T) {
			sampler, err := newPatternSampler(test.pattern)
			require.NoError(t, err)
			value, ok := sampler.sample(test.min, test.max)
			require.True(t, ok)
			require.True(t, sampler.fullMatch(value), "%q does not fully match", value)
			length := utf8.RuneCountInString(value)
			require.GreaterOrEqual(t, length, test.min)
			require.LessOrEqual(t, length, test.max)
		})
	}
}

func TestPatternSamplerRejectsLookahead(t *testing.T) {
	_, err := newPatternSampler(`^(?!aws:).*`)
	require.Error(t, err)
}

func TestPatternBreakerAvoidsUnanchoredMatches(t *testing.T) {
	// Unanchored, the pattern matches any value with one letter in it.
	re := regexp.MustCompile(`[a-z]`)
	broken, ok := patternBreaker(re, 3, 10)
	require.True(t, ok)
	require.False(t, re.MatchString(broken), "%q", broken)
	require.GreaterOrEqual(t, len(broken), 3)

	_, ok = patternBreaker(regexp.MustCompile(`.*`), 1, 10)
	require.False(t, ok, "nothing breaks a pattern that matches everything")
}

func TestSaltLettersAreDistinct(t *testing.T) {
	seen := map[string]bool{}
	for n := range 2000 {
		salt := saltLetters(n)
		require.False(t, seen[salt], "salt %d repeats %q", n, salt)
		require.Equal(t, strings.ToLower(salt), salt)
		seen[salt] = true
	}
}
