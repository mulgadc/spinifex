package ecr

import (
	"strings"
	"testing"
)

func TestAccountBucket(t *testing.T) {
	if got := AccountBucket("123456789012"); got != "ecr-123456789012" {
		t.Fatalf("AccountBucket = %q, want ecr-123456789012", got)
	}
}

func TestKeyHelpers(t *testing.T) {
	cases := []struct {
		name string
		got  string
		want string
	}{
		{"repo meta", RepoMetaKey("team/app"), "repos/team/app/_meta.json"},
		{"tag", TagKey("team/app", "v1"), "repos/team/app/manifests/by-tag/v1"},
		{"manifest", ManifestKey("team/app", "sha256:abcd"), "repos/team/app/manifests/by-digest/sha256-abcd"},
		{"index", IndexKey("team/app"), "repos/team/app/manifests/index.json"},
		{"upload", UploadKey("u-1"), "uploads/u-1"},
	}
	for _, c := range cases {
		if c.got != c.want {
			t.Errorf("%s = %q, want %q", c.name, c.got, c.want)
		}
	}
}

func TestBlobKey(t *testing.T) {
	cases := []struct {
		name   string
		digest string
		want   string
	}{
		{"sha256 prefixed", "sha256:abcdef0123", "blobs/ab/sha256-abcdef0123"},
		{"bare hex", "abcdef0123", "blobs/ab/abcdef0123"},
		{"short digest", "a", "blobs/a/a"},
	}
	for _, c := range cases {
		if got := BlobKey(c.digest); got != c.want {
			t.Errorf("%s: BlobKey(%q) = %q, want %q", c.name, c.digest, got, c.want)
		}
	}
}

// TestObjectKeysAreColonFree guards the SigV4 invariant: predastore rejects
// signed requests whose object key contains ':', so digest-derived object keys
// must be sanitized.
func TestObjectKeysAreColonFree(t *testing.T) {
	digest := "sha256:" + strings.Repeat("a", 64)
	for _, k := range []string{BlobKey(digest), ManifestKey("team/app", digest)} {
		if strings.Contains(k, ":") {
			t.Errorf("object key must not contain ':': %q", k)
		}
	}
}
