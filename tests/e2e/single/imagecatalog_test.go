//go:build e2e

package single

import (
	"crypto/tls"
	"fmt"
	"maps"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/mulgadc/spinifex/spinifex/operator/imagecatalog"
	"github.com/mulgadc/spinifex/tests/e2e/harness"
	"github.com/stretchr/testify/assert"
)

// catalogHeadTimeout bounds one HEAD. Several of these hosts are CDN-fronted
// and occasionally slow to first byte, but none is slow to answer a HEAD.
const catalogHeadTimeout = 30 * time.Second

// catalogRetryDelay spaces the one retry of an unreachable host, long enough
// to ride out a CDN edge that dropped a connection.
const catalogRetryDelay = 10 * time.Second

// ownImageDomain is the domain of the catalog hosts we run ourselves.
const ownImageDomain = "mulgadc.com"

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
//
// imagecatalog.WithheldImages is deliberately not walked: those are the entries whose
// artifact is known to be unpublished, so HEADing them would assert a failure
// we already know about and hide the ones we do not.
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

	for _, name := range slices.Sorted(maps.Keys(imagecatalog.AvailableImages)) {
		img := imagecatalog.AvailableImages[name]
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			// Exactly one of the two must be set, or a verified import is not
			// possible; resolveDigest refuses the entry rather than importing
			// it unverified, so an empty pair is a latent hard failure.
			hasURL, hasPinned := img.Checksum != "", img.ChecksumDigest != ""
			assert.NotEqualf(t, hasURL, hasPinned,
				"%s must set exactly one of Checksum (sums file URL) or ChecksumDigest (pinned digest); got url=%q pinned=%q",
				name, img.Checksum, img.ChecksumDigest)
			assert.NotEmptyf(t, img.ChecksumType, "%s sets no ChecksumType", name)
			if hasPinned {
				// A pinned digest is only safe on a URL that names one build.
				// "latest" upstreams re-point, and the next release would fail
				// every import of this entry until someone edited the catalog.
				assert.NotContainsf(t, strings.ToLower(img.URL), "latest",
					"%s pins a digest against a 'latest' URL, whose bytes change under it", name)
			}

			size, err := headCatalogURL(t, client, img.URL)
			if !assert.NoErrorf(t, err, "catalog image %q is unreachable; `spx admin images import --name %s` would fail for every operator", name, name) {
				return
			}
			assert.NotZerof(t, size, "%s answered but reports a zero-length body", name)
			harness.Detail(t, "image", name, "bytes", fmt.Sprintf("%d", size))

			if hasURL {
				_, err := headCatalogURL(t, client, img.Checksum)
				assert.NoErrorf(t, err, "checksum file for %q is unreachable, so the import cannot verify and will refuse", name)
			}
		})
	}
}

// headCatalogURL HEADs one catalog URL, retrying once when the host could not
// be reached. A third-party host still unreachable after that skips the
// subtest, since its outage says nothing about the catalog; our own host fails.
// A host that answers non-2xx always fails.
func headCatalogURL(t *testing.T, client *http.Client, rawURL string) (int64, error) {
	t.Helper()
	harness.Step(t, "HEAD %s", rawURL)
	size, err := harness.HeadContentLength(client, rawURL, catalogHeadTimeout)
	if !harness.UpstreamUnavailable(err) || ownCatalogHost(rawURL) {
		return size, err
	}
	harness.Step(t, "HEAD %s: %v; retrying once in %s", rawURL, err, catalogRetryDelay)
	time.Sleep(catalogRetryDelay)
	size, err = harness.HeadContentLength(client, rawURL, catalogHeadTimeout)
	if harness.UpstreamUnavailable(err) {
		t.Skipf("third-party host for %s is unreachable twice (%v): an upstream outage, not a catalog fault, so this entry is unverified this run",
			rawURL, err)
	}
	return size, err
}

// ownCatalogHost reports whether rawURL is served from a host we run, whose
// outage is ours to fix rather than an upstream's to wait out.
func ownCatalogHost(rawURL string) bool {
	u, err := url.Parse(rawURL)
	if err != nil {
		return false
	}
	host := u.Hostname()
	return host == ownImageDomain || strings.HasSuffix(host, "."+ownImageDomain)
}
