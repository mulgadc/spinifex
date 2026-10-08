package taskdefinition

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestKeys(t *testing.T) {
	assert.Equal(t, "taskdef-families/", FamiliesPrefix())
	assert.Equal(t, "taskdef-families/nginx/latest-rev", LatestRevKey("nginx"))
	assert.Equal(t, "taskdef-families/nginx/revs/", RevsPrefix("nginx"))
	assert.Equal(t, "taskdef-families/nginx/revs/3", RevKey("nginx", 3))
	assert.Contains(t, RevKey("f", 1), RevsPrefix("f"))
}

func TestParseRef(t *testing.T) {
	for ref, want := range map[string]struct {
		family string
		rev    int
	}{
		"app":     {"app", 0},
		"app:7":   {"app", 7},
		" app:7 ": {"app", 7},
		"arn:aws:ecs:ap-southeast-2:123456789012:task-definition/app:3": {"app", 3},
		"app:x": {"app", 0},
		"":      {"", 0},
	} {
		family, rev := ParseRef(ref)
		assert.Equal(t, want.family, family, ref)
		assert.Equal(t, want.rev, rev, ref)
	}
}
