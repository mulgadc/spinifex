package awsmodel

//test:in-package — checks generated inputs against the unexported model
//shapes, and the unexported pattern sampler and salt helpers.

import (
	"crypto/tls"
	"crypto/x509"
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

var testRequestOptions = RequestOptions{AccountID: "123456789012", Region: "ap-southeast-2", AccessKeyID: "AKIAIOSFODNN7EXAMPLE"}

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
			m.inputViolations(shape.Key.Shape, path+mapKeyPath, key, violations)
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
	re := regexp.MustCompile(re2Pattern(pattern))
	testPatterns.Store(pattern, re)
	return re
}

// TestGeneratedRequestsBreakOnlyTheirConstraint checks every modelled
// operation: acceptance cases come first and break nothing, a rejection breaks
// only its named constraint, and every case encodes.
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
				rejecting := false
				for _, request := range plan.Cases {
					require.False(t, rejecting && request.Acceptance(), "%s acceptance %s follows a rejection", operationName, request.Path)
					rejecting = !request.Acceptance()
					if service != S3 {
						_, err := EncodeRequest(service, operationName, request.Input)
						require.NoError(t, err, "%s %s %s encodes", operationName, request.Constraint, request.Path)
					}
					var violations []constraintViolation
					model.inputViolations(operation.Input.Shape, "$", request.Input, &violations)
					for member := range request.Input {
						if !slices.Contains(input.Required, member) && !slices.Contains(conditionallyRequired[service][operationName], member) {
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

func TestGenerateRequestsSeedsConditionallyRequiredMembers(t *testing.T) {
	for service, operations := range conditionallyRequired {
		model, err := Load(service)
		require.NoError(t, err)
		for operationName, members := range operations {
			operation, ok := model.Operation(operationName)
			require.True(t, ok, "%s %s is not modelled", service, operationName)
			input := model.shapes[operation.Input.Shape]
			for _, member := range members {
				require.Contains(t, input.Members, member, "%s %s", service, operationName)
				require.NotContains(t, input.Required, member, "%s %s now models %s as required", service, operationName, member)
			}

			plan, err := GenerateRequests(service, operationName, testRequestOptions)
			require.NoError(t, err)
			for _, request := range plan.Cases {
				for _, member := range members {
					require.NotEqual(t, member, request.Member, "%s %s", service, operationName)
					require.Contains(t, request.Input, member, "%s %s %s %s", service, operationName, request.Constraint, request.Path)
				}
			}
		}
	}

	plan, err := GenerateRequests(RDS, "CreateDBInstance", testRequestOptions)
	require.NoError(t, err)
	require.Equal(t, int64(20), plan.Cases[0].Input["AllocatedStorage"])
	plan, err = GenerateRequests(EC2, "CreateCapacityReservation", testRequestOptions)
	require.NoError(t, err)
	require.Equal(t, "ap-southeast-2a", plan.Cases[0].Input["AvailabilityZone"])

	// A constraint inside a seeded member is judged against the base request.
	plan, err = GenerateRequests(ECS, "CreateCapacityProvider", testRequestOptions)
	require.NoError(t, err)
	nested := 0
	for _, request := range plan.Cases {
		if strings.HasPrefix(request.Path, "$.autoScalingGroupProvider.") {
			nested++
			require.Empty(t, request.Member, request.Path)
		}
	}
	require.NotZero(t, nested)
}

func TestGenerateRequestsTargetsFixturesOutsideCreateAndDelete(t *testing.T) {
	options := testRequestOptions
	options.Fixtures = map[string]string{"RoleName": "fixture"}
	for operation, wantFixture := range map[string]bool{"TagRole": true, "CreateRole": false, "DeleteRole": false} {
		plan, err := GenerateRequests(IAM, operation, options)
		require.NoError(t, err)
		for _, request := range plan.Cases {
			if request.Path == "$.RoleName" {
				continue
			}
			require.Equal(t, wantFixture, request.Input["RoleName"] == "fixture", "%s %s %s", operation, request.Constraint, request.Path)
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

func TestGenerateRequestsBreaksEachConstraintKind(t *testing.T) {
	plan, err := GenerateRequests(IAM, "CreateRole", testRequestOptions)
	require.NoError(t, err)
	var rejections []string
	for _, request := range plan.Cases {
		if !request.Acceptance() {
			rejections = append(rejections, string(request.Constraint)+" "+request.Path)
		}
	}
	for _, want := range []string{
		"required $.RoleName", "length $.RoleName", "pattern $.RoleName",
		"range $.MaxSessionDuration", "length $.Tags", "required $.Tags[0].Key",
	} {
		require.Contains(t, rejections, want)
	}

	plan, err = GenerateRequests(EC2, "CreateVolume", testRequestOptions)
	require.NoError(t, err)
	var enums []string
	for _, request := range plan.Cases {
		if request.Constraint == ConstraintEnum {
			enums = append(enums, request.Path)
		}
	}
	require.Contains(t, enums, "$.VolumeType")
}

func TestGenerateRequestsBreaksMapKeys(t *testing.T) {
	plan, err := GenerateRequests(EKS, "TagResource", testRequestOptions)
	require.NoError(t, err)
	var keyRejections []Constraint
	for _, request := range plan.Cases {
		if request.Path == "$.tags"+mapKeyPath {
			keyRejections = append(keyRejections, request.Constraint)
			tags := request.Input["tags"].(map[string]any)
			require.Len(t, tags, 1, "the key is renamed, not added")
		}
	}
	require.Equal(t, []Constraint{ConstraintLength, ConstraintLength}, keyRejections, "below min and above max")
}

func TestGenerateRequestsRecordsUnbrokenConstraints(t *testing.T) {
	plan, err := GenerateRequests(EKS, "DescribeCluster", testRequestOptions)
	require.NoError(t, err)
	require.Contains(t, plan.Unbroken, Skip{Path: "$.name", Reason: "a required URI label cannot be omitted"})

	plan, err = GenerateRequests(IAM, "CreateRole", testRequestOptions)
	require.NoError(t, err)
	require.Contains(t, plan.Unbroken, Skip{Path: "$.AssumeRolePolicyDocument", Reason: "max length 131072 is above the generated limit"})
	require.Contains(t, plan.DeclaredErrors, "InvalidInput")
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
		{ECR, "TagResource", "resourceArn", regexp.MustCompile(`^arn:aws:ecr:ap-southeast-2:123456789012:repository/`)},
		{EKS, "TagResource", "resourceArn", regexp.MustCompile(`^arn:aws:eks:ap-southeast-2:123456789012:cluster/`)},
		{EKS, "AssociateAccessPolicy", "policyArn", regexp.MustCompile(`^arn:aws:eks::aws:cluster-access-policy/AmazonEKSViewPolicy$`)},
		{EKS, "CreateAddon", "addonName", regexp.MustCompile(`^aws-ebs-csi-driver$`)},
		{EKS, "CreateAccessEntry", "type", regexp.MustCompile(`^STANDARD$`)},
		{ElasticLoadBalancingV2, "CreateTargetGroup", "ProtocolVersion", regexp.MustCompile(`^HTTP1$`)},
		{EC2, "CopyImage", "SourceRegion", regexp.MustCompile(`^ap-southeast-2$`)},
		{STS, "GetAccessKeyInfo", "AccessKeyId", regexp.MustCompile(`^AKIAIOSFODNN7EXAMPLE$`)},
		{IAM, "DeleteAccessKey", "AccessKeyId", regexp.MustCompile(`^AKIA[0-9A-F]{16}$`)},
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

func TestGenerateRequestsHintsNestedMembers(t *testing.T) {
	plan, err := GenerateRequests(ElasticLoadBalancingV2, "CreateTargetGroup", testRequestOptions)
	require.NoError(t, err)
	for _, request := range plan.Cases {
		if request.Acceptance() && request.Member == "Matcher" {
			require.Equal(t, "200", request.Input["Matcher"].(map[string]any)["HttpCode"])
			return
		}
	}
	t.Fatal("no acceptance request sets Matcher")
}

func TestGenerateRequestsImportsAVerifiableCertificate(t *testing.T) {
	plan, err := GenerateRequests(ACM, "ImportCertificate", testRequestOptions)
	require.NoError(t, err)
	var input map[string]any
	for _, request := range plan.Cases {
		if request.Acceptance() && request.Member == "CertificateChain" {
			input = request.Input
		}
	}
	require.NotNil(t, input, "no acceptance request sets CertificateChain")

	pair, err := tls.X509KeyPair(input["Certificate"].([]byte), input["PrivateKey"].([]byte))
	require.NoError(t, err)
	leaf, err := x509.ParseCertificate(pair.Certificate[0])
	require.NoError(t, err)
	roots := x509.NewCertPool()
	require.True(t, roots.AppendCertsFromPEM(input["CertificateChain"].([]byte)))
	_, err = leaf.Verify(x509.VerifyOptions{Roots: roots, DNSName: "spx.example.com"})
	require.NoError(t, err)
}

func TestRE2EquivalentsMatchTheModelledSemantics(t *testing.T) {
	modelled := map[string]bool{}
	for _, service := range Services() {
		model, err := Load(service)
		require.NoError(t, err)
		for _, shape := range model.shapes {
			modelled[shape.Pattern] = true
		}
	}
	for pattern := range re2Equivalents {
		require.True(t, modelled[pattern], "no model uses %s", pattern)
	}

	domain := testPattern(`^(\*\.)?(((?!-)[A-Za-z0-9-]{0,62}[A-Za-z0-9])\.)+((?!-)[A-Za-z0-9-]{1,62}[A-Za-z0-9])$`)
	for _, name := range []string{"example.com", "*.example.com", "a.b.co", "x-y.example.com", "a." + strings.Repeat("b", 63)} {
		require.True(t, domain.MatchString(name), name)
	}
	for _, name := range []string{"example", "-a.com", "a-.com", "a.c", "a.-com", "a.com-", "a..com", "*.com.*", strings.Repeat("a", 64) + ".com"} {
		require.False(t, domain.MatchString(name), name)
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
