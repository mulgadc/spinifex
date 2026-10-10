package taskdefinition

import (
	"fmt"
	"strconv"
	"strings"
)

// FamiliesPrefix returns the KV key prefix under which all task-definition
// families live. Task definitions are account-scoped, not cluster-scoped.
func FamiliesPrefix() string {
	return "taskdef-families/"
}

// LatestRevKey returns the KV key holding a family's latest revision number.
// Read-modify-written on register.
func LatestRevKey(family string) string {
	return fmt.Sprintf("taskdef-families/%s/latest-rev", family)
}

// RevsPrefix returns the KV key prefix under which all revisions of a
// task-definition family live.
func RevsPrefix(family string) string {
	return fmt.Sprintf("taskdef-families/%s/revs/", family)
}

// RevKey returns the KV key for a specific revision of a task-definition family.
func RevKey(family string, rev int) string {
	return fmt.Sprintf("%s%d", RevsPrefix(family), rev)
}

// ARN returns the task-definition ARN for a resolved numeric revision, family:rev.
func ARN(region, accountID, family string, rev int) string {
	return RefARN(region, accountID, family, strconv.Itoa(rev))
}

// RefARN spells the revision verbatim, so a reference whose revision is
// not yet resolved can render it as a wildcard.
func RefARN(region, accountID, family, revision string) string {
	return fmt.Sprintf("arn:aws:ecs:%s:%s:task-definition/%s:%s", region, accountID, family, revision)
}

// ParseRef splits "family", "family:rev" or an ARN into (family, rev).
// rev is 0 when unspecified (caller resolves to latest).
func ParseRef(ref string) (string, int) {
	ref = strings.TrimSpace(ref)
	if i := strings.LastIndex(ref, "task-definition/"); i >= 0 {
		ref = ref[i+len("task-definition/"):]
	}
	family := ref
	rev := 0
	if i := strings.LastIndexByte(ref, ':'); i >= 0 {
		family = ref[:i]
		if n, err := strconv.Atoi(ref[i+1:]); err == nil {
			rev = n
		}
	}
	return family, rev
}
