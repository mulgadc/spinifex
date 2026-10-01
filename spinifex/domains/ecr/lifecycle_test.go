package ecr

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func img(digest string, pushedDaysAgo int, tags ...string) LifecycleImage {
	return LifecycleImage{
		Digest:   digest,
		Tags:     tags,
		PushedAt: time.Now().UTC().AddDate(0, 0, -pushedDaysAgo),
	}
}

func digests(exps []LifecycleExpiry) []string {
	out := make([]string, len(exps))
	for i, e := range exps {
		out[i] = e.Digest
	}
	return out
}

func TestParseLifecyclePolicy_Valid(t *testing.T) {
	doc, err := ParseLifecyclePolicy([]byte(`{"rules":[
		{"rulePriority":1,"selection":{"tagStatus":"untagged","countType":"sinceImagePushed","countUnit":"days","countNumber":14},"action":{"type":"expire"}}
	]}`))
	require.NoError(t, err)
	require.Len(t, doc.Rules, 1)
	assert.Equal(t, 1, doc.Rules[0].RulePriority)
}

func TestParseLifecyclePolicy_Invalid(t *testing.T) {
	cases := map[string]string{
		"malformed json":                   `{`,
		"unknown field":                    `{"rules":[{"rulePriority":1,"selection":{"tagStatus":"any","countType":"imageCountMoreThan","countNumber":1,"bogus":1},"action":{"type":"expire"}}]}`,
		"empty rules":                      `{"rules":[]}`,
		"zero priority":                    `{"rules":[{"rulePriority":0,"selection":{"tagStatus":"any","countType":"imageCountMoreThan","countNumber":1},"action":{"type":"expire"}}]}`,
		"dup priority":                     `{"rules":[{"rulePriority":1,"selection":{"tagStatus":"any","countType":"imageCountMoreThan","countNumber":1},"action":{"type":"expire"}},{"rulePriority":1,"selection":{"tagStatus":"untagged","countType":"imageCountMoreThan","countNumber":1},"action":{"type":"expire"}}]}`,
		"tagged needs prefix":              `{"rules":[{"rulePriority":1,"selection":{"tagStatus":"tagged","countType":"imageCountMoreThan","countNumber":1},"action":{"type":"expire"}}]}`,
		"untagged forbids prefix":          `{"rules":[{"rulePriority":1,"selection":{"tagStatus":"untagged","tagPrefixList":["v"],"countType":"imageCountMoreThan","countNumber":1},"action":{"type":"expire"}}]}`,
		"sinceImagePushed needs days unit": `{"rules":[{"rulePriority":1,"selection":{"tagStatus":"any","countType":"sinceImagePushed","countUnit":"hours","countNumber":1},"action":{"type":"expire"}}]}`,
		"imageCountMoreThan forbids unit":  `{"rules":[{"rulePriority":1,"selection":{"tagStatus":"any","countType":"imageCountMoreThan","countUnit":"days","countNumber":1},"action":{"type":"expire"}}]}`,
		"bad count type":                   `{"rules":[{"rulePriority":1,"selection":{"tagStatus":"any","countType":"nope","countNumber":1},"action":{"type":"expire"}}]}`,
		"zero count number":                `{"rules":[{"rulePriority":1,"selection":{"tagStatus":"any","countType":"imageCountMoreThan","countNumber":0},"action":{"type":"expire"}}]}`,
		"non-expire action":                `{"rules":[{"rulePriority":1,"selection":{"tagStatus":"any","countType":"imageCountMoreThan","countNumber":1},"action":{"type":"retain"}}]}`,
	}
	for name, policy := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := ParseLifecyclePolicy([]byte(policy))
			assert.ErrorIs(t, err, ErrInvalidLifecyclePolicy)
		})
	}
}

func TestEvaluate_UntagedSinceImagePushed(t *testing.T) {
	policy := []byte(`{"rules":[{"rulePriority":1,"selection":{"tagStatus":"untagged","countType":"sinceImagePushed","countUnit":"days","countNumber":7},"action":{"type":"expire"}}]}`)
	images := []LifecycleImage{
		img("sha256:old-untagged", 10),     // untagged, 10d old -> expire
		img("sha256:new-untagged", 2),      // untagged, 2d old -> keep
		img("sha256:old-tagged", 10, "v1"), // tagged -> not selected
	}
	exp, err := EvaluateLifecyclePolicy(policy, images, time.Now().UTC())
	require.NoError(t, err)
	assert.Equal(t, []string{"sha256:old-untagged"}, digests(exp))
}

func TestEvaluate_ImageCountMoreThan_KeepsNewest(t *testing.T) {
	policy := []byte(`{"rules":[{"rulePriority":1,"selection":{"tagStatus":"any","countType":"imageCountMoreThan","countNumber":2},"action":{"type":"expire"}}]}`)
	images := []LifecycleImage{
		img("sha256:a", 1),
		img("sha256:b", 2),
		img("sha256:c", 3),
		img("sha256:d", 4),
	}
	exp, err := EvaluateLifecyclePolicy(policy, images, time.Now().UTC())
	require.NoError(t, err)
	// Newest two (a,b) kept; oldest two (c,d) expire.
	assert.ElementsMatch(t, []string{"sha256:c", "sha256:d"}, digests(exp))
}

func TestEvaluate_TaggedPrefix(t *testing.T) {
	policy := []byte(`{"rules":[{"rulePriority":1,"selection":{"tagStatus":"tagged","tagPrefixList":["release-"],"countType":"imageCountMoreThan","countNumber":1},"action":{"type":"expire"}}]}`)
	images := []LifecycleImage{
		img("sha256:r1", 1, "release-1"),
		img("sha256:r2", 2, "release-2"),
		img("sha256:dev", 3, "dev-1"), // prefix mismatch -> not selected
	}
	exp, err := EvaluateLifecyclePolicy(policy, images, time.Now().UTC())
	require.NoError(t, err)
	// Of the two release- images, keep newest (r1), expire r2; dev untouched.
	assert.Equal(t, []string{"sha256:r2"}, digests(exp))
}

func TestEvaluate_PriorityClaimsImageOnce(t *testing.T) {
	// Rule 1 expires untagged older than 1 day; rule 2 (any) keeps newest 0.
	// An untagged old image must be claimed by rule 1 only, appearing once.
	policy := []byte(`{"rules":[
		{"rulePriority":1,"selection":{"tagStatus":"untagged","countType":"sinceImagePushed","countUnit":"days","countNumber":1},"action":{"type":"expire"}},
		{"rulePriority":2,"selection":{"tagStatus":"any","countType":"imageCountMoreThan","countNumber":1},"action":{"type":"expire"}}
	]}`)
	images := []LifecycleImage{
		img("sha256:untagged-old", 5),
		img("sha256:tagged-new", 0, "latest"),
		img("sha256:tagged-old", 5, "v1"),
	}
	exp, err := EvaluateLifecyclePolicy(policy, images, time.Now().UTC())
	require.NoError(t, err)
	// rule1: untagged-old. rule2 over remaining {tagged-new, tagged-old}: keep
	// newest (tagged-new), expire tagged-old. untagged-old not double-counted.
	assert.ElementsMatch(t, []string{"sha256:untagged-old", "sha256:tagged-old"}, digests(exp))

	seen := map[string]int{}
	for _, e := range exp {
		seen[e.Digest]++
	}
	for d, n := range seen {
		assert.Equal(t, 1, n, "digest %s expired more than once", d)
	}
}

func TestEvaluate_AppliedRulePriorityRecorded(t *testing.T) {
	policy := []byte(`{"rules":[{"rulePriority":5,"selection":{"tagStatus":"any","countType":"sinceImagePushed","countUnit":"days","countNumber":1},"action":{"type":"expire"}}]}`)
	exp, err := EvaluateLifecyclePolicy(policy, []LifecycleImage{img("sha256:x", 5, "t")}, time.Now().UTC())
	require.NoError(t, err)
	require.Len(t, exp, 1)
	assert.Equal(t, 5, exp[0].RulePriority)
}
