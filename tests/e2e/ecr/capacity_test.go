//go:build e2e

package ecr

import "testing"

// TestECRCapacityLoad is the predastore capacity/load leg of Sprint 2f (parallel
// crane push of many large images, asserting predastore throughput with no
// manifest corruption). Deferred: it depends on predastore fixes mulga-siv-359
// (':'-in-key SigV4 canonicalization) and mulga-siv-356 (404 NoSuchBucket),
// which are owned elsewhere. Unskip once those land.
func TestECRCapacityLoad(t *testing.T) {
	t.Skip("deferred: blocked on predastore mulga-siv-359 / mulga-siv-356")
}
