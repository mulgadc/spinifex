package cluster

import (
	"os"
	"regexp"
	"slices"
	"sort"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var eksSubjectLiteral = regexp.MustCompile(`"(eks\.[A-Z]\w+)"`)

// subjectsIn returns the eks request subjects named as string literals in a
// source file, the gateway adapter or the daemon's subscription table.
func subjectsIn(t *testing.T, path string) []string {
	t.Helper()
	src, err := os.ReadFile(path)
	require.NoError(t, err)
	var out []string
	for _, m := range eksSubjectLiteral.FindAllStringSubmatch(string(src), -1) {
		if m[1] == TokenVerifySubject {
			continue
		}
		out = append(out, m[1])
	}
	sort.Strings(out)
	return slices.Compact(out)
}

// TestS8_EKSSubjectBudgets is the ADR-0003:S8 subject table for EKS: every eks
// request subject the gateway sends and the daemon serves is classified by the
// work its handler does, a request-scaled handler keeps the ordinary budget,
// and the handlers still holding a request for resource-scaled work are named
// here so none is added, or mistaken for conformant, silently.
func TestS8_EKSSubjectBudgets(t *testing.T) {
	t.Parallel()
	classified := make([]string, 0, len(eksSubjects))
	for s := range eksSubjects {
		classified = append(classified, s)
	}
	sort.Strings(classified)

	assert.Equal(t, classified, subjectsIn(t, "service_nats.go"),
		"every subject the gateway adapter sends must be classified, and nothing else")
	assert.Equal(t, classified, subjectsIn(t, "../../../runtime/daemon/daemon.go"),
		"every eks subject the daemon serves must be classified, and nothing else")

	for _, s := range classified {
		assert.Equal(t, defaultTimeout, requestTimeout(s),
			"%s: no budget is raised to cover a handler's work", s)
	}

	var gaps []string
	for s, w := range eksSubjects {
		if w == resourceScaledGap {
			gaps = append(gaps, s)
		}
	}
	sort.Strings(gaps)
	assert.Equal(t, []string{
		"eks.CreateCluster",
		"eks.DeleteNodegroup",
		"eks.RestoreSnapshot",
		"eks.UpdateNodegroupConfig",
	}, gaps, "the known non-conformant handlers; moving one out needs its handler fixed, adding one needs a decision")
	assert.Equal(t, requestScaled, eksSubjects["eks.DeleteCluster"],
		"ADR-0003:S8: DeleteCluster records the delete and returns")
}
