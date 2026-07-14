package ovn

import (
	"fmt"

	"github.com/mulgadc/spinifex/spinifex/network/ovn/nbdb"
)

// aclKey builds a content-addressable key covering every semantic ACL field so a
// current NB row set can be compared against desired specs without minting UUIDs.
func aclKey(direction string, priority int, match, action string, log bool, name, severity string) string {
	return fmt.Sprintf("%s\x1f%d\x1f%s\x1f%s\x1f%t\x1f%s\x1f%s",
		direction, priority, match, action, log, name, severity)
}

func aclRowKey(a *nbdb.ACL) string {
	name, severity := "", ""
	if a.Name != nil {
		name = *a.Name
	}
	if a.Severity != nil {
		severity = *a.Severity
	}
	return aclKey(a.Direction, a.Priority, a.Match, a.Action, a.Log, name, severity)
}

func aclSpecKey(s ACLSpec) string {
	return aclKey(s.Direction, s.Priority, s.Match, s.Action, s.Log, s.Name, s.Severity)
}

// ACLSetEqual reports whether rows and specs describe the same ACL multiset by
// semantic content (ignoring UUID and ExternalIDs). Shared by LiveClient and the
// mock so both no-op an unchanged ReplaceACLs instead of churning row UUIDs.
func ACLSetEqual(rows []nbdb.ACL, specs []ACLSpec) bool {
	if len(rows) != len(specs) {
		return false
	}
	counts := make(map[string]int, len(specs))
	for _, s := range specs {
		counts[aclSpecKey(s)]++
	}
	for i := range rows {
		k := aclRowKey(&rows[i])
		if counts[k] == 0 {
			return false
		}
		counts[k]--
	}
	return true
}
