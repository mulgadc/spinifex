package ebsprovider

import (
	"encoding/json"
	"fmt"
)

// VolumeParameters is what the control plane sends in
// CreateVolumeRequest.Parameters. A provider that has no use for a field
// ignores it; one that maps it, such as a performance tier, reads it here.
type VolumeParameters struct {
	// VolumeType is the EC2 volume type the caller asked for, such as gp3.
	VolumeType string `json:"volume_type,omitempty"`
}

// NewVolumeParameters encodes the parameters for a volume of volumeType.
func NewVolumeParameters(volumeType string) json.RawMessage {
	// A struct of one string field cannot fail to marshal.
	raw, _ := json.Marshal(VolumeParameters{VolumeType: volumeType})
	return raw
}

// ParseVolumeParameters decodes raw, which may be empty. Malformed parameters
// are refused as invalid rather than read as an absent volume type.
func ParseVolumeParameters(raw json.RawMessage) (VolumeParameters, error) {
	var params VolumeParameters
	if len(raw) == 0 {
		return params, nil
	}
	if err := json.Unmarshal(raw, &params); err != nil {
		return VolumeParameters{}, fmt.Errorf("%w: volume parameters: %v", ErrInvalidArgument, err)
	}
	return params, nil
}
