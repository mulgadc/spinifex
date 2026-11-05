//go:build lambdatestlink

//test:in-package — reads the unexported ldflags image pin
package awsgw

import (
	"os"
	"testing"

	"github.com/mulgadc/spinifex/spinifex/runtime/compute/lambda/testlink"
)

// TestLambdaImageDigestPin runs from make build-lambda-testlink with the
// daemon's ldflags: the gateway carries the same image digest as the daemon.
func TestLambdaImageDigestPin(t *testing.T) {
	want := os.Getenv("LAMBDA_IMAGE_DIGEST")
	if want == "" {
		t.Skip("run by make build-lambda-testlink with a pinned image digest")
	}
	if lambdaImageDigest != want || !testlink.ValidDigest(lambdaImageDigest) {
		t.Fatalf("pinned image digest %q, want %q", lambdaImageDigest, want)
	}
}
