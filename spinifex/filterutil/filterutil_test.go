package filterutil

import (
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
)

func TestParseFilters_NilInput(t *testing.T) {
	result, err := ParseFilters(nil, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result != nil {
		t.Fatalf("expected nil, got %v", result)
	}
}

func TestParseFilters_EmptyInput(t *testing.T) {
	result, err := ParseFilters([]*ec2.Filter{}, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result != nil {
		t.Fatalf("expected nil, got %v", result)
	}
}

func TestParseFilters_ValidFilters(t *testing.T) {
	valid := map[string]bool{"vpc-id": true, "state": true}
	filters := []*ec2.Filter{
		{Name: aws.String("vpc-id"), Values: []*string{aws.String("vpc-1"), aws.String("vpc-2")}},
		{Name: aws.String("state"), Values: []*string{aws.String("running")}},
	}

	result, err := ParseFilters(filters, valid)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(result) != 2 {
		t.Fatalf("expected 2 filter keys, got %d", len(result))
	}
	if len(result["vpc-id"]) != 2 {
		t.Fatalf("expected 2 vpc-id values, got %d", len(result["vpc-id"]))
	}
	if result["state"][0] != "running" {
		t.Fatalf("expected state=running, got %s", result["state"][0])
	}
}

func TestParseFilters_InvalidFilterName(t *testing.T) {
	valid := map[string]bool{"vpc-id": true}
	filters := []*ec2.Filter{
		{Name: aws.String("bogus-filter"), Values: []*string{aws.String("x")}},
	}

	_, err := ParseFilters(filters, valid)
	if err == nil {
		t.Fatal("expected error for invalid filter name")
	}
}

func TestParseFilters_TagFiltersAlwaysAccepted(t *testing.T) {
	valid := map[string]bool{"vpc-id": true}
	filters := []*ec2.Filter{
		{Name: aws.String("tag:Environment"), Values: []*string{aws.String("prod")}},
	}

	result, err := ParseFilters(filters, valid)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(result["tag:Environment"]) != 1 {
		t.Fatalf("expected 1 tag filter value, got %d", len(result["tag:Environment"]))
	}
}

func TestParseFilters_NilNameSkipped(t *testing.T) {
	valid := map[string]bool{"vpc-id": true}
	filters := []*ec2.Filter{
		{Name: nil, Values: []*string{aws.String("x")}},
		{Name: aws.String("vpc-id"), Values: []*string{aws.String("vpc-1")}},
	}

	result, err := ParseFilters(filters, valid)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(result) != 1 {
		t.Fatalf("expected 1 filter key, got %d", len(result))
	}
}

func TestParseFilters_NilValuesSkipped(t *testing.T) {
	valid := map[string]bool{"state": true}
	filters := []*ec2.Filter{
		{Name: aws.String("state"), Values: []*string{aws.String("running"), nil, aws.String("stopped")}},
	}

	result, err := ParseFilters(filters, valid)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(result["state"]) != 2 {
		t.Fatalf("expected 2 state values, got %d", len(result["state"]))
	}
}

func TestMatchesAny_EmptyFilterValues(t *testing.T) {
	if !MatchesAny(nil, "anything") {
		t.Fatal("expected true for empty filter values")
	}
	if !MatchesAny([]string{}, "anything") {
		t.Fatal("expected true for empty filter values")
	}
}

func TestMatchesAny_ExactMatch(t *testing.T) {
	if !MatchesAny([]string{"running", "stopped"}, "running") {
		t.Fatal("expected match for 'running'")
	}
	if MatchesAny([]string{"running", "stopped"}, "terminated") {
		t.Fatal("expected no match for 'terminated'")
	}
}

func TestMatchesAny_WildcardStar(t *testing.T) {
	if !MatchesAny([]string{"*"}, "anything") {
		t.Fatal("expected * to match anything")
	}
}

func TestMatchesAny_WildcardPrefix(t *testing.T) {
	if !MatchesAny([]string{"prod-*"}, "prod-web") {
		t.Fatal("expected prod-* to match prod-web")
	}
	if MatchesAny([]string{"prod-*"}, "staging-web") {
		t.Fatal("expected prod-* to not match staging-web")
	}
}

func TestMatchesAny_WildcardSuffix(t *testing.T) {
	if !MatchesAny([]string{"*-web"}, "prod-web") {
		t.Fatal("expected *-web to match prod-web")
	}
	if MatchesAny([]string{"*-web"}, "prod-api") {
		t.Fatal("expected *-web to not match prod-api")
	}
}

func TestMatchesAny_WildcardMiddle(t *testing.T) {
	if !MatchesAny([]string{"prod-*-web"}, "prod-us-east-web") {
		t.Fatal("expected prod-*-web to match prod-us-east-web")
	}
	if MatchesAny([]string{"prod-*-web"}, "prod-us-east-api") {
		t.Fatal("expected prod-*-web to not match prod-us-east-api")
	}
}

func TestMatchesAny_MultipleWildcards(t *testing.T) {
	if !MatchesAny([]string{"*prod*web*"}, "my-prod-us-web-server") {
		t.Fatal("expected *prod*web* to match")
	}
	if MatchesAny([]string{"*prod*web*"}, "my-staging-us-api-server") {
		t.Fatal("expected *prod*web* to not match")
	}
}

func TestMatchesAny_EmptyStringMatches(t *testing.T) {
	if !MatchesAny([]string{""}, "") {
		t.Fatal("expected empty string to match empty string")
	}
	if MatchesAny([]string{""}, "notempty") {
		t.Fatal("expected empty string to not match non-empty")
	}
}

func TestMatchesTags_NoTagFilters(t *testing.T) {
	filters := map[string][]string{
		"vpc-id": {"vpc-1"},
	}
	tags := map[string]string{"Environment": "prod"}
	if !MatchesTags(filters, tags) {
		t.Fatal("expected true when no tag: filters present")
	}
}

func TestMatchesTags_MatchingSingleTag(t *testing.T) {
	filters := map[string][]string{
		"tag:Environment": {"prod"},
	}
	tags := map[string]string{"Environment": "prod"}
	if !MatchesTags(filters, tags) {
		t.Fatal("expected tag match")
	}
}

func TestMatchesTags_NonMatchingTag(t *testing.T) {
	filters := map[string][]string{
		"tag:Environment": {"prod"},
	}
	tags := map[string]string{"Environment": "staging"}
	if MatchesTags(filters, tags) {
		t.Fatal("expected no tag match")
	}
}

func TestMatchesTags_MissingTag(t *testing.T) {
	filters := map[string][]string{
		"tag:Environment": {"prod"},
	}
	tags := map[string]string{"Team": "infra"}
	if MatchesTags(filters, tags) {
		t.Fatal("expected no match when tag key is missing")
	}
}

func TestMatchesTags_NilTags(t *testing.T) {
	filters := map[string][]string{
		"tag:Environment": {"prod"},
	}
	if MatchesTags(filters, nil) {
		t.Fatal("expected no match with nil tags")
	}
}

func TestMatchesTags_MultipleTagFiltersAND(t *testing.T) {
	filters := map[string][]string{
		"tag:Environment": {"prod"},
		"tag:Team":        {"infra", "platform"},
	}
	// Matches both: Environment=prod AND Team in {infra, platform}
	tags := map[string]string{"Environment": "prod", "Team": "infra"}
	if !MatchesTags(filters, tags) {
		t.Fatal("expected match for both tag filters")
	}

	// Fails second filter
	tags2 := map[string]string{"Environment": "prod", "Team": "frontend"}
	if MatchesTags(filters, tags2) {
		t.Fatal("expected no match when second tag doesn't match")
	}
}

func TestMatchesTags_WildcardValues(t *testing.T) {
	filters := map[string][]string{
		"tag:Name": {"prod-*"},
	}
	tags := map[string]string{"Name": "prod-web-01"}
	if !MatchesTags(filters, tags) {
		t.Fatal("expected wildcard tag match")
	}
}

func TestMatchesTags_EmptyFilters(t *testing.T) {
	if !MatchesTags(nil, nil) {
		t.Fatal("expected true for nil filters")
	}
	if !MatchesTags(map[string][]string{}, nil) {
		t.Fatal("expected true for empty filters")
	}
}

func TestEC2TagsToMap(t *testing.T) {
	tags := []*ec2.Tag{
		{Key: aws.String("Env"), Value: aws.String("prod")},
		{Key: aws.String("Team"), Value: aws.String("infra")},
	}
	m := EC2TagsToMap(tags)
	if m["Env"] != "prod" || m["Team"] != "infra" {
		t.Fatalf("unexpected map: %v", m)
	}
}

func TestEC2TagsToMap_NilAndEmpty(t *testing.T) {
	if EC2TagsToMap(nil) != nil {
		t.Fatal("expected nil for nil tags")
	}
	if EC2TagsToMap([]*ec2.Tag{}) != nil {
		t.Fatal("expected nil for empty tags")
	}
}

func TestMatchWildcard_OverlappingSuffixNotDoubled(t *testing.T) {
	// Pattern "a*b*ab" — the suffix "ab" must not be consumed by the
	// middle "b" match, leaving nothing for the suffix anchor.
	if !MatchesAny([]string{"a*b*ab"}, "abab") {
		t.Fatal("expected a*b*ab to match abab")
	}
	if MatchesAny([]string{"a*b*ab"}, "ab") {
		t.Fatal("expected a*b*ab to NOT match ab (suffix overlap)")
	}
}

// TestMatchWildcard_SingleCharacterAndEscapes covers the EC2 filter grammar
// beyond "*": "?" for exactly one character, and "\" escaping the next byte.
func TestMatchWildcard_SingleCharacterAndEscapes(t *testing.T) {
	tests := []struct {
		pattern string
		value   string
		want    bool
	}{
		// "?" alone.
		{"a?c", "abc", true},
		{"a?c", "ac", false},
		{"a?c", "abbc", false},

		// "?" never matches the empty string.
		{"abc?", "abc", false},
		{"?", "", false},
		{"?", "a", true},

		// "?" at pattern start and end.
		{"?bc", "abc", true},
		{"?bc", "bc", false},
		{"ab?", "abc", true},

		// "?" combined with "*".
		{"a?*c", "abxc", true},
		{"a?*c", "ac", false},
		{"web?-*", "web1-prod", true},
		{"web?-*", "web-prod", false},

		// Escaped metacharacters match themselves and nothing else.
		{`a\*c`, "a*c", true},
		{`a\*c`, "abc", false},
		{`a\?c`, "a?c", true},
		{`a\?c`, "abc", false},
		{`a\\c`, `a\c`, true},
		{`a\\c`, "abc", false},
		{`ab\`, `ab\`, true},

		// An unescaped "*" in the pattern stays a wildcard even when the value
		// contains a literal "*" at the same offset.
		{"a*", "a*c", true},
		{"a*c", "a*c", true},

		// Backtracking.
		{"a*b?c", "axxbyc", true},
		{"*?", "a", true},
		{"a*a*a*b", "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", false},
	}

	for _, tt := range tests {
		if got := MatchWildcard(tt.pattern, tt.value); got != tt.want {
			t.Errorf("MatchWildcard(%q, %q) = %v, want %v", tt.pattern, tt.value, got, tt.want)
		}
	}
}

// TestMatchesTags_SingleCharacterWildcard confirms the fix reaches the tag
// filter path callers actually use.
func TestMatchesTags_SingleCharacterWildcard(t *testing.T) {
	filters := map[string][]string{"tag:Name": {"web?"}}
	if !MatchesTags(filters, map[string]string{"Name": "web1"}) {
		t.Fatal("expected tag:Name=web? to match web1")
	}
	if MatchesTags(filters, map[string]string{"Name": "web"}) {
		t.Fatal("expected tag:Name=web? to NOT match web")
	}
}

func TestMatchWildcard_TwoPartPattern(t *testing.T) {
	// Simple two-part: prefix + suffix, no middle parts.
	if !MatchesAny([]string{"a*a"}, "aa") {
		t.Fatal("expected a*a to match aa")
	}
	if !MatchesAny([]string{"a*a"}, "aXa") {
		t.Fatal("expected a*a to match aXa")
	}
	if MatchesAny([]string{"a*a"}, "a") {
		t.Fatal("expected a*a to NOT match a")
	}
}
