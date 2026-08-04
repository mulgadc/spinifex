//go:build e2e

package ecr

import "testing"

// TestECRCapacityLoad pushes many large images with crane and asserts
// Predastore throughput without manifest corruption. It remains disabled until
// the required SigV4 canonicalization and NoSuchBucket behavior are available.
func TestECRCapacityLoad(t *testing.T) {
	t.Skip("deferred: blocked on predastore fixes")
}
