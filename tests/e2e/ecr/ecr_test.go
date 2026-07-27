//go:build e2e

package ecr

import (
	"fmt"
	"time"
)

// uniqueRepo returns a collision-resistant repo name scoped to a test.
func uniqueRepo(prefix string) string {
	return fmt.Sprintf("e2e/%s-%d", prefix, time.Now().UnixNano())
}
