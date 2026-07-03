package admin

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/s3"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	handlers_ec2_image "github.com/mulgadc/spinifex/spinifex/handlers/ec2/image"
	"github.com/mulgadc/spinifex/spinifex/objectstore"
	"github.com/mulgadc/spinifex/spinifex/utils"
	"github.com/mulgadc/viperblock/viperblock"
)

// SystemOwnerAlias is the fixed owner alias written to AMI config on promotion.
// It must be a non-account-ID string so callerCanReadAMI treats the AMI as a
// system image visible to all accounts.
const SystemOwnerAlias = "system"

// PromoteImageOpts configures PromoteSystemImage.
type PromoteImageOpts struct {
	ImageID string
}

// PromoteImageResult summarises what changed after a successful promotion.
type PromoteImageResult struct {
	// PreviousOwner is the ImageOwnerAlias before promotion (the account ID).
	PreviousOwner string
}

// PromoteImageInput is the NATS message body for the spinifex.image.promote topic.
type PromoteImageInput struct {
	ImageID string `json:"ImageID"`
}

// PromoteImageOutput is the NATS reply for the spinifex.image.promote topic.
type PromoteImageOutput struct {
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

	meta, err := readAMIConfig(store, bucket, opts.ImageID)
	switch {
	case err == nil:
		// ok
	case objectstore.IsNoSuchKeyError(err):
		return nil, errors.New(awserrors.ErrorInvalidAMIIDNotFound)
	case errors.Is(err, handlers_ec2_image.ErrCorruptAMIConfig):
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

	if err := writeAMIConfig(store, bucket, opts.ImageID, meta); err != nil {
		slog.Error("PromoteSystemImage: write AMI config", "imageId", opts.ImageID, "err", err)
		return nil, errors.New(awserrors.ErrorServerInternal)
	}

	slog.Info("PromoteSystemImage completed", "imageId", opts.ImageID, "previousOwner", prev, "newOwner", SystemOwnerAlias)
	return &PromoteImageResult{PreviousOwner: prev}, nil
}

// writeAMIConfig persists updated AMIMetadata to {imageID}/config.json,
// preserving the VBState wrapper used by GetAMIConfig.
func writeAMIConfig(store objectstore.ObjectStore, bucket, imageID string, meta viperblock.AMIMetadata) error {
	state := viperblock.VBState{
		VolumeConfig: viperblock.VolumeConfig{
			AMIMetadata: meta,
		},
	}
	data, err := json.Marshal(state)
	if err != nil {
		return err
	}
	_, err = store.PutObject(&s3.PutObjectInput{
		Bucket:      aws.String(bucket),
		Key:         aws.String(imageID + "/config.json"),
		Body:        bytes.NewReader(data),
		ContentType: aws.String("application/json"),
	})
	return err
}

// GetAMIMetadata reads and returns the AMIMetadata for the given image ID.
// Returns ErrorInvalidAMIIDNotFound for missing or corrupt configs.
func GetAMIMetadata(store objectstore.ObjectStore, bucket, imageID string) (viperblock.AMIMetadata, error) {
	meta, err := readAMIConfig(store, bucket, imageID)
	switch {
	case err == nil:
		return meta, nil
	case objectstore.IsNoSuchKeyError(err):
		return viperblock.AMIMetadata{}, errors.New(awserrors.ErrorInvalidAMIIDNotFound)
	case errors.Is(err, handlers_ec2_image.ErrCorruptAMIConfig):
		return viperblock.AMIMetadata{}, errors.New(awserrors.ErrorInvalidAMIIDNotFound)
	default:
		return viperblock.AMIMetadata{}, errors.New(awserrors.ErrorServerInternal)
	}
}
