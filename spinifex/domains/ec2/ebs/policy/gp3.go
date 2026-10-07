// Package ebspolicy owns EC2 EBS product policy that the control plane exposes.
package ebspolicy

import (
	"strings"

	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
)

// VolumeTypeGP3 is the only EBS volume type this platform currently serves.
const VolumeTypeGP3 = "gp3"

// SupportedVolumeTypes lists the EBS volume types CreateVolume accepts.
var SupportedVolumeTypes = []string{VolumeTypeGP3}

// ValidateVolumeType accepts an empty type (the gp3 default) or a supported one,
// matched case-insensitively as EC2 does, and answers UnknownVolumeType otherwise.
func ValidateVolumeType(volumeType string) error {
	if volumeType == "" {
		return nil
	}
	for _, t := range SupportedVolumeTypes {
		if strings.EqualFold(volumeType, t) {
			return nil
		}
	}
	return awserrors.Errorf(awserrors.ErrorUnknownVolumeType,
		"Unsupported volume type '%s' for volume creation. Supported volume types: %s.",
		volumeType, strings.Join(SupportedVolumeTypes, ", "))
}

const (
	// GP3 IOPS envelope (AWS): 3000 baseline on any size, up to 500 IOPS/GiB,
	// capped at 16000.
	DefaultGP3IOPS = 3000
	MaxGP3IOPS     = 16000
	GP3IOPSPerGiB  = 500

	// GP3 throughput envelope (AWS): 125 MiB/s baseline, 1000 MiB/s ceiling,
	// flat range independent of volume size.
	DefaultGP3Throughput = 125
	MaxGP3Throughput     = 1000
)
