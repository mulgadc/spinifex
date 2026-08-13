package admin

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/mulgadc/spinifex/spinifex/awserrors"
	"github.com/mulgadc/spinifex/spinifex/ebsmetadata"
	"github.com/mulgadc/spinifex/spinifex/objectstore"
	"github.com/mulgadc/spinifex/spinifex/utils"
)

// SystemOwnerAlias is the fixed owner alias written to AMI config on promotion.
// It must be a non-account-ID string so callerCanReadAMI treats the AMI as a
// system image visible to all accounts.
const SystemOwnerAlias = "system"

// PromoteImageOpts is the input for PromoteSystemImage and the NATS message
// body for the spinifex.image.promote topic.
type PromoteImageOpts struct {
	ImageID string `json:"ImageID"`
}

// PromoteImageResult summarises what changed after a successful promotion and
// is also the NATS reply for the spinifex.image.promote topic.
type PromoteImageResult struct {
	// PreviousOwner is the ImageOwnerAlias before promotion (the account ID).
	PreviousOwner string `json:"PreviousOwner"`
}

// PromoteSystemImage promotes an account-owned AMI to a system image by
// rewriting its ImageOwnerAlias to SystemOwnerAlias. After the call the AMI
// is immediately visible to all accounts via DescribeImages.
//
// Guards:
//   - ImageID must have "ami-" prefix
//   - config.json must exist and parse cleanly
//   - AMI must currently be account-owned; already-system AMIs are rejected
func PromoteSystemImage(store objectstore.ObjectStore, bucket string, opts PromoteImageOpts) (*PromoteImageResult, error) {
	if !strings.HasPrefix(opts.ImageID, "ami-") {
		return nil, errors.New(awserrors.ErrorInvalidAMIIDMalformed)
	}

	meta, err := readAMI(store, bucket, opts.ImageID)
	switch {
	case err == nil:
		// ok
	case objectstore.IsNoSuchKeyError(err):
		return nil, errors.New(awserrors.ErrorInvalidAMIIDNotFound)
	case errors.Is(err, ebsmetadata.ErrCorruptDocument):
		return nil, errors.New(awserrors.ErrorInvalidAMIIDNotFound)
	default:
		slog.Error("PromoteSystemImage: read AMI config", "imageId", opts.ImageID, "err", err)
		return nil, errors.New(awserrors.ErrorServerInternal)
	}

	if meta.ImageOwnerAlias == "" || !utils.IsAccountID(meta.ImageOwnerAlias) {
		return nil, fmt.Errorf("%s is already a system-owned AMI (owner: %q); promotion not allowed", opts.ImageID, meta.ImageOwnerAlias)
	}

	prev := meta.ImageOwnerAlias
	meta.ImageOwnerAlias = SystemOwnerAlias

	if err := ebsmetadata.NewStore(store, bucket).PutAMI(context.Background(), meta); err != nil {
		slog.Error("PromoteSystemImage: write AMI document", "imageId", opts.ImageID, "err", err)
		return nil, errors.New(awserrors.ErrorServerInternal)
	}

	slog.Info("PromoteSystemImage completed", "imageId", opts.ImageID, "previousOwner", prev, "newOwner", SystemOwnerAlias)
	return &PromoteImageResult{PreviousOwner: prev}, nil
}

// GetAMIMetadata reads and returns the control-plane document for the given
// image ID. Returns ErrorInvalidAMIIDNotFound for missing or corrupt documents.
func GetAMIMetadata(store objectstore.ObjectStore, bucket, imageID string) (ebsmetadata.AMI, error) {
	meta, err := readAMI(store, bucket, imageID)
	switch {
	case err == nil:
		return meta, nil
	case objectstore.IsNoSuchKeyError(err):
		return ebsmetadata.AMI{}, errors.New(awserrors.ErrorInvalidAMIIDNotFound)
	case errors.Is(err, ebsmetadata.ErrCorruptDocument):
		return ebsmetadata.AMI{}, errors.New(awserrors.ErrorInvalidAMIIDNotFound)
	default:
		return ebsmetadata.AMI{}, errors.New(awserrors.ErrorServerInternal)
	}
}
