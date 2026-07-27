package gateway_ecr

import (
	"net/http"
	"net/url"
	"strings"
)

// ECR IAM action names, unprefixed. Combine with policy.IAMAction("ecr", ...)
// to build the "ecr:Action" string the identity-policy evaluator expects.
const (
	ActionDescribeRepositories        = "DescribeRepositories"
	ActionListImages                  = "ListImages"
	ActionBatchCheckLayerAvailability = "BatchCheckLayerAvailability"
	ActionGetDownloadUrlForLayer      = "GetDownloadUrlForLayer"
	ActionInitiateLayerUpload         = "InitiateLayerUpload"
	ActionUploadLayerPart             = "UploadLayerPart"
	ActionCompleteLayerUpload         = "CompleteLayerUpload"
	ActionBatchGetImage               = "BatchGetImage"
	ActionPutImage                    = "PutImage"
	ActionBatchDeleteImage            = "BatchDeleteImage"
)

// ResourceScope identifies which repository ARN an ActionRequirement is
// evaluated against.
type ResourceScope int

const (
	// ScopeNone means the operation needs a validly rehydrated principal but
	// no resource-specific policy decision (the bare /v2/ version probe).
	ScopeNone ResourceScope = iota
	// ScopeAccountWildcard is the account-wide repository/* ARN (catalog listing).
	ScopeAccountWildcard
	// ScopeDestination is the operation's target repository.
	ScopeDestination
	// ScopeSource is the cross-repository mount's source repository.
	ScopeSource
)

// ActionRequirement is one ECR IAM action an operation must be allowed
// before dispatch, and the resource scope it is evaluated against.
type ActionRequirement struct {
	Action string
	Scope  ResourceScope
}

// ClassifiedOperation is the result of mapping an OCI /v2/* request to its
// required ECR authorization. Repo is the destination repository name (empty
// for account-scoped or repo-less operations). Source and MountDigest are
// populated only for a cross-repository blob-mount attempt (?mount=&from=).
type ClassifiedOperation struct {
	Repo        string
	Source      string
	MountDigest string
	// Requirements lists every ActionRequirement that must independently
	// evaluate allow before the operation may dispatch. Empty for the bare
	// version probe, which still requires a resolved principal but no
	// resource-specific decision.
	Requirements []ActionRequirement
}

// ClassifyOperation maps method and urlPath — the request's full path as
// Registry.serve sees it ("/v2/..." including the mount prefix) — to the ECR
// operation it names. ok is false for any method/path pair Registry does not
// implement, so a future dispatch branch added without a matching case here
// fails authorization closed instead of silently dispatching unauthorized.
// query supplies the mount/digest parameters for a blob-upload-start request.
func ClassifyOperation(method, urlPath string, query url.Values) (ClassifiedOperation, bool) {
	path := strings.TrimPrefix(urlPath, "/v2/")
	if path == urlPath {
		path = strings.TrimPrefix(urlPath, "/v2")
	}

	if path == "" {
		if method == http.MethodGet {
			return ClassifiedOperation{}, true
		}
		return ClassifiedOperation{}, false
	}

	if path == "_catalog" {
		if method != http.MethodGet {
			return ClassifiedOperation{}, false
		}
		return ClassifiedOperation{
			Requirements: []ActionRequirement{{ActionDescribeRepositories, ScopeAccountWildcard}},
		}, true
	}

	name, kind, ref, ok := splitV2Path(path)
	if !ok {
		return ClassifiedOperation{}, false
	}

	switch kind {
	case "tags":
		return classifyTagsOperation(method, name, ref)
	case "blobs":
		return classifyBlobOperation(method, name, ref, query)
	case "manifests":
		return classifyManifestOperation(method, name, ref)
	default:
		return ClassifiedOperation{}, false
	}
}

func classifyTagsOperation(method, name, ref string) (ClassifiedOperation, bool) {
	if method != http.MethodGet || ref != "list" {
		return ClassifiedOperation{}, false
	}
	return ClassifiedOperation{Repo: name, Requirements: []ActionRequirement{{ActionListImages, ScopeDestination}}}, true
}

// classifyBlobOperation mirrors Registry.routeBlobs' dispatch exactly, so
// every method/ref combination it accepts has a matching authorization case.
func classifyBlobOperation(method, name, ref string, query url.Values) (ClassifiedOperation, bool) {
	switch {
	case ref == "uploads/" || ref == "uploads":
		if method != http.MethodPost {
			return ClassifiedOperation{}, false
		}
		op := ClassifiedOperation{
			Repo:         name,
			Requirements: []ActionRequirement{{ActionInitiateLayerUpload, ScopeDestination}},
		}
		// A cross-repository mount additionally requires source-layer read
		// permission. Both mount and from must be present — a mount digest with
		// no source (or vice versa) is not a mount attempt, matching Registry's
		// own "mount miss falls through to a normal upload" behavior.
		mountDigest, from := query.Get("mount"), query.Get("from")
		if mountDigest != "" && from != "" {
			op.Source = from
			op.MountDigest = mountDigest
			op.Requirements = append(op.Requirements, ActionRequirement{ActionBatchCheckLayerAvailability, ScopeSource})
		}
		return op, true
	case strings.HasPrefix(ref, "uploads/"):
		if strings.TrimPrefix(ref, "uploads/") == "" {
			return ClassifiedOperation{}, false
		}
		switch method {
		case http.MethodPatch:
			return ClassifiedOperation{Repo: name, Requirements: []ActionRequirement{{ActionUploadLayerPart, ScopeDestination}}}, true
		case http.MethodPut:
			return ClassifiedOperation{Repo: name, Requirements: []ActionRequirement{{ActionCompleteLayerUpload, ScopeDestination}}}, true
		case http.MethodDelete:
			// ECR defines no abort-upload IAM action; UploadLayerPart is the
			// permission governing mutation of an in-progress upload, so cancel
			// requires it too. Pinned compatibility decision, not an oversight.
			return ClassifiedOperation{Repo: name, Requirements: []ActionRequirement{{ActionUploadLayerPart, ScopeDestination}}}, true
		default:
			return ClassifiedOperation{}, false
		}
	default:
		// ref is a digest.
		switch method {
		case http.MethodHead:
			return ClassifiedOperation{Repo: name, Requirements: []ActionRequirement{{ActionBatchCheckLayerAvailability, ScopeDestination}}}, true
		case http.MethodGet:
			return ClassifiedOperation{Repo: name, Requirements: []ActionRequirement{{ActionGetDownloadUrlForLayer, ScopeDestination}}}, true
		default:
			return ClassifiedOperation{}, false
		}
	}
}

func classifyManifestOperation(method, name, reference string) (ClassifiedOperation, bool) {
	if reference == "" {
		return ClassifiedOperation{}, false
	}
	switch method {
	case http.MethodHead, http.MethodGet:
		return ClassifiedOperation{Repo: name, Requirements: []ActionRequirement{{ActionBatchGetImage, ScopeDestination}}}, true
	case http.MethodPut:
		return ClassifiedOperation{Repo: name, Requirements: []ActionRequirement{{ActionPutImage, ScopeDestination}}}, true
	case http.MethodDelete:
		return ClassifiedOperation{Repo: name, Requirements: []ActionRequirement{{ActionBatchDeleteImage, ScopeDestination}}}, true
	default:
		return ClassifiedOperation{}, false
	}
}
