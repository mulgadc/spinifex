package gateway_ecrapi

import (
	"github.com/mulgadc/spinifex/spinifex/awserrors"
)

// ImageNotFoundReason is the failureReason AWS reports for an imageId that
// names no image in BatchGetImage and BatchDeleteImage.
const ImageNotFoundReason = "Requested image not found"

// RepositoryNotFoundError returns RepositoryNotFoundException naming the
// repository and registry, as AWS does.
func RepositoryNotFoundError(registryID, name string) error {
	return awserrors.Errorf(awserrors.ErrorRepositoryNotFound,
		"The repository with name '%s' does not exist in the registry with id '%s'", name, registryID)
}

// RepositoryAlreadyExistsError returns RepositoryAlreadyExistsException naming
// the repository and registry, as AWS does.
func RepositoryAlreadyExistsError(registryID, name string) error {
	return awserrors.Errorf(awserrors.ErrorRepositoryAlreadyExists,
		"The repository with name '%s' already exists in the registry with id '%s'", name, registryID)
}

// LifecyclePolicyNotFoundError returns LifecyclePolicyNotFoundException naming
// the repository and registry, as AWS does.
func LifecyclePolicyNotFoundError(registryID, name string) error {
	return awserrors.Errorf(awserrors.ErrorLifecyclePolicyNotFound,
		"Lifecycle policy does not exist for the repository with name '%s' in the registry with id '%s'", name, registryID)
}

// RepositoryPolicyNotFoundError returns RepositoryPolicyNotFoundException
// naming the repository and registry, as AWS does.
func RepositoryPolicyNotFoundError(registryID, name string) error {
	return awserrors.Errorf(awserrors.ErrorRepositoryPolicyNotFound,
		"Repository policy does not exist for the repository with name '%s' in the registry with id '%s'", name, registryID)
}

// ImageNotFoundError returns ImageNotFoundException naming the image, the
// repository and the registry; an absent digest or tag prints as 'null'.
func ImageNotFoundError(registryID, name, digest, tag string) error {
	return awserrors.Errorf(awserrors.ErrorImageNotFound,
		"The image with imageId {imageDigest:'%s', imageTag:'%s'} does not exist within the repository with name '%s' in the registry with id '%s'",
		orNull(digest), orNull(tag), name, registryID)
}

func orNull(s string) string {
	if s == "" {
		return "null"
	}
	return s
}
