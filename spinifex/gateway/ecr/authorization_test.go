package gateway_ecr

import (
	"net/http"
	"net/url"
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestClassifyOperation_RouteMatrix pins the plan's OCI request -> ECR action
// -> resource table exactly. Every row here must match the plan doc's table;
// changing an entry without a corresponding plan-doc update is a regression.
func TestClassifyOperation_RouteMatrix(t *testing.T) {
	cases := []struct {
		name   string
		method string
		path   string
		query  url.Values
		want   ClassifiedOperation
	}{
		{
			name:   "GET /v2/ version probe",
			method: http.MethodGet,
			path:   "/v2/",
			want:   ClassifiedOperation{},
		},
		{
			name:   "GET /v2/_catalog",
			method: http.MethodGet,
			path:   "/v2/_catalog",
			want: ClassifiedOperation{
				Requirements: []ActionRequirement{{ActionDescribeRepositories, ScopeAccountWildcard}},
			},
		},
		{
			name:   "GET tags list",
			method: http.MethodGet,
			path:   "/v2/team/app/tags/list",
			want: ClassifiedOperation{
				Repo:         "team/app",
				Requirements: []ActionRequirement{{ActionListImages, ScopeDestination}},
			},
		},
		{
			name:   "HEAD blob",
			method: http.MethodHead,
			path:   "/v2/team/app/blobs/sha256:abc",
			want: ClassifiedOperation{
				Repo:         "team/app",
				Requirements: []ActionRequirement{{ActionBatchCheckLayerAvailability, ScopeDestination}},
			},
		},
		{
			name:   "GET blob",
			method: http.MethodGet,
			path:   "/v2/team/app/blobs/sha256:abc",
			want: ClassifiedOperation{
				Repo:         "team/app",
				Requirements: []ActionRequirement{{ActionGetDownloadUrlForLayer, ScopeDestination}},
			},
		},
		{
			name:   "POST start upload",
			method: http.MethodPost,
			path:   "/v2/team/app/blobs/uploads/",
			want: ClassifiedOperation{
				Repo:         "team/app",
				Requirements: []ActionRequirement{{ActionInitiateLayerUpload, ScopeDestination}},
			},
		},
		{
			name:   "PATCH upload chunk",
			method: http.MethodPatch,
			path:   "/v2/team/app/blobs/uploads/upload-1",
			want: ClassifiedOperation{
				Repo:         "team/app",
				Requirements: []ActionRequirement{{ActionUploadLayerPart, ScopeDestination}},
			},
		},
		{
			name:   "PUT finish upload",
			method: http.MethodPut,
			path:   "/v2/team/app/blobs/uploads/upload-1",
			want: ClassifiedOperation{
				Repo:         "team/app",
				Requirements: []ActionRequirement{{ActionCompleteLayerUpload, ScopeDestination}},
			},
		},
		{
			name:   "DELETE cancel upload uses UploadLayerPart (no abort action exists)",
			method: http.MethodDelete,
			path:   "/v2/team/app/blobs/uploads/upload-1",
			want: ClassifiedOperation{
				Repo:         "team/app",
				Requirements: []ActionRequirement{{ActionUploadLayerPart, ScopeDestination}},
			},
		},
		{
			name:   "HEAD manifest",
			method: http.MethodHead,
			path:   "/v2/team/app/manifests/latest",
			want: ClassifiedOperation{
				Repo:         "team/app",
				Requirements: []ActionRequirement{{ActionBatchGetImage, ScopeDestination}},
			},
		},
		{
			name:   "GET manifest",
			method: http.MethodGet,
			path:   "/v2/team/app/manifests/latest",
			want: ClassifiedOperation{
				Repo:         "team/app",
				Requirements: []ActionRequirement{{ActionBatchGetImage, ScopeDestination}},
			},
		},
		{
			name:   "PUT manifest",
			method: http.MethodPut,
			path:   "/v2/team/app/manifests/latest",
			want: ClassifiedOperation{
				Repo:         "team/app",
				Requirements: []ActionRequirement{{ActionPutImage, ScopeDestination}},
			},
		},
		{
			name:   "DELETE manifest",
			method: http.MethodDelete,
			path:   "/v2/team/app/manifests/sha256:abc",
			want: ClassifiedOperation{
				Repo:         "team/app",
				Requirements: []ActionRequirement{{ActionBatchDeleteImage, ScopeDestination}},
			},
		},
		{
			name:   "cross-repo mount requires destination initiate + source read",
			method: http.MethodPost,
			path:   "/v2/team/app/blobs/uploads/",
			query:  url.Values{"mount": {"sha256:abc"}, "from": {"team/base"}},
			want: ClassifiedOperation{
				Repo:        "team/app",
				Source:      "team/base",
				MountDigest: "sha256:abc",
				Requirements: []ActionRequirement{
					{ActionInitiateLayerUpload, ScopeDestination},
					{ActionBatchCheckLayerAvailability, ScopeSource},
				},
			},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			q := c.query
			if q == nil {
				q = url.Values{}
			}
			got, ok := ClassifyOperation(c.method, c.path, q)
			assert.True(t, ok, "expected %s %s to classify", c.method, c.path)
			assert.Equal(t, c.want, got)
		})
	}
}

// TestClassifyOperation_MountRequiresBothParams pins that a mount digest or
// source repo alone is not a mount attempt: it classifies as a plain upload
// start (Registry itself falls through to a normal upload on a mount miss).
func TestClassifyOperation_MountRequiresBothParams(t *testing.T) {
	cases := []struct {
		name  string
		query url.Values
	}{
		{"digest without source", url.Values{"mount": {"sha256:abc"}}},
		{"source without digest", url.Values{"from": {"team/base"}}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, ok := ClassifyOperation(http.MethodPost, "/v2/team/app/blobs/uploads/", c.query)
			assert.True(t, ok)
			assert.Empty(t, got.Source)
			assert.Empty(t, got.MountDigest)
			assert.Equal(t, []ActionRequirement{{ActionInitiateLayerUpload, ScopeDestination}}, got.Requirements)
		})
	}
}

// TestClassifyOperation_UnsupportedNeverClassifies verifies that unsupported
// methods and malformed paths never authorize an operation: ok must be
// false, so no ActionRequirement can leak through unmapped.
func TestClassifyOperation_UnsupportedNeverClassifies(t *testing.T) {
	cases := []struct {
		name   string
		method string
		path   string
	}{
		{"POST /v2/ unsupported method", http.MethodPost, "/v2/"},
		{"malformed path, no marker", http.MethodGet, "/v2/team/app"},
		{"empty repo name", http.MethodGet, "/v2//tags/list"},
		{"unknown kind segment", http.MethodGet, "/v2/team/app/unknown/list"},
		{"tags list wrong method", http.MethodPost, "/v2/team/app/tags/list"},
		{"tags wrong ref", http.MethodGet, "/v2/team/app/tags/other"},
		{"blob PUT unsupported", http.MethodPut, "/v2/team/app/blobs/sha256:abc"},
		{"blob upload empty uuid", http.MethodPatch, "/v2/team/app/blobs/uploads/"},
		{"blob upload GET unsupported", http.MethodGet, "/v2/team/app/blobs/uploads/upload-1"},
		{"manifest POST unsupported", http.MethodPost, "/v2/team/app/manifests/latest"},
		{"manifest empty reference", http.MethodGet, "/v2/team/app/manifests/"},
		{"catalog wrong method", http.MethodPost, "/v2/_catalog"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, ok := ClassifyOperation(c.method, c.path, url.Values{})
			assert.False(t, ok, "expected %s %s to be unclassified", c.method, c.path)
			assert.Equal(t, ClassifiedOperation{}, got)
		})
	}
}
