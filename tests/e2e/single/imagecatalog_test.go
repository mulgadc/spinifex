//go:build e2e

package single

import (
	"context"
	"crypto/tls"
	"fmt"
	"maps"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/mulgadc/spinifex/spinifex/utils"
	"github.com/mulgadc/spinifex/tests/e2e/harness"
	"github.com/stretchr/testify/assert"
)

// catalogHeadTimeout bounds one HEAD. Several of these hosts are CDN-fronted
// and occasionally slow to first byte, but none is slow to answer a HEAD.
const catalogHeadTimeout = 30 * time.Second

// runImageCatalogReachable HEADs every URL in the image catalog.
//
// The catalog names artifacts on hosts nobody here controls — Debian, Ubuntu,
// Alpine, Rocky, Oracle and our own iso.mulgadc.com. An upstream that retires
// a build, moves a path or lets a certificate lapse breaks `spx admin images
// import` for a customer, and nothing else in the suite would notice: every
// other test launches from an AMI that is already imported.
//
// This runs in the general single-node suite rather than nightly-only on
// purpose. It is the check most likely to catch a problem that is not ours,
// and the sooner it is seen the smaller the window in which a released binary
// points at a dead URL.
func runImageCatalogReachable(t *testing.T, _ *Fixture) {
	harness.Phase(t, "Single — Image catalog URLs still answer")

	client := &http.Client{
		Timeout: catalogHeadTimeout,
		// The catalog is pinned to https and every redirect must stay there;
		// a downgrade would fetch image bytes in the clear.
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 10 {
				return fmt.Errorf("stopped after 10 redirects")
			}
			if req.URL.Scheme != "https" {
				return fmt.Errorf("refusing non-https redirect to %s", req.URL.Redacted())
			}
			return nil
		},
		Transport: &http.Transport{TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12}},
	}

	for _, name := range slices.Sorted(maps.Keys(utils.AvailableImages)) {
		img := utils.AvailableImages[name]
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			harness.Step(t, "HEAD %s", img.URL)
			size, err := headOK(client, img.URL)
			if !assert.NoErrorf(t, err, "catalog image %q is unreachable; `spx admin images import --name %s` would fail for every operator", name, name) {
				return
			}
			assert.NotZerof(t, size, "%s answered but reports a zero-length body", name)
			harness.Detail(t, "image", name, "bytes", fmt.Sprintf("%d", size))

			// Exactly one of the two must be set, or a verified import is not
			// possible; resolveDigest refuses the entry rather than importing
			// it unverified, so an empty pair is a latent hard failure.
			hasURL, hasPinned := img.Checksum != "", img.ChecksumDigest != ""
			assert.NotEqualf(t, hasURL, hasPinned,
				"%s must set exactly one of Checksum (sums file URL) or ChecksumDigest (pinned digest); got url=%q pinned=%q",
				name, img.Checksum, img.ChecksumDigest)
			assert.NotEmptyf(t, img.ChecksumType, "%s sets no ChecksumType", name)

			switch {
			case hasURL:
				harness.Step(t, "HEAD %s", img.Checksum)
				_, err := headOK(client, img.Checksum)
				assert.NoErrorf(t, err, "checksum file for %q is unreachable, so the import cannot verify and will refuse", name)
			case hasPinned:
				// A pinned digest is only safe on a URL that names one build.
				// "latest" upstreams re-point, and the next release would fail
				// every import of this entry until someone edited the catalog.
				assert.NotContainsf(t, strings.ToLower(img.URL), "latest",
					"%s pins a digest against a 'latest' URL, whose bytes change under it", name)
			}
		})
	}
}

// headOK issues a HEAD and returns Content-Length, erroring on any non-2xx.
// A HEAD and not a ranged GET: the point is whether the artifact is still
// published, and pulling bytes for twenty-odd multi-hundred-megabyte images
// would make this the longest test in the suite.
func headOK(client *http.Client, rawURL string) (int64, error) {
	ctx, cancel := context.WithTimeout(context.Background(), catalogHeadTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodHead, rawURL, nil)
	if err != nil {
		return 0, fmt.Errorf("build request: %w", err)
	}
	resp, err := client.Do(req)
	if err != nil {
		return 0, err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return 0, fmt.Errorf("HTTP %s", resp.Status)
	}
	return resp.ContentLength, nil
}
