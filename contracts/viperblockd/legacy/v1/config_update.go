// Package viperblocklegacyv1 contains the versioned, transitional wire
// contracts between Spinifex's legacy control-plane routes and viperblockd.
package viperblocklegacyv1

import "encoding/json"

const (
	// ConfigUpdateSubject is the legacy queue-group route for an encrypted
	// volume configuration update when no mounted volume owns the request.
	ConfigUpdateSubject = "ebs.config"
)

// VolumeConfigUpdateSubject returns the legacy route for an update handled by
// the node with a live mount of volume. Volume remains unvalidated here: route
// authorization and volume-name validation are responsibilities of the
// consumer, as they were before this contract was extracted.
func VolumeConfigUpdateSubject(volume string) string {
	return ConfigUpdateSubject + "." + volume
}

// EBSConfigUpdateRequest carries a control-plane VolumeConfig update for an
// encrypted volume. config.json is a sealed VBState; only the master-key holder
// (viperblockd) can reseal it, so the EC2 edge ships the new config here instead
// of rewriting the object directly. VolumeConfig is a marshaled
// viperblock.VolumeConfig (RawMessage keeps this contract dependency-free).
type EBSConfigUpdateRequest struct {
	Volume       string          `json:"Volume"`
	VolumeConfig json.RawMessage `json:"VolumeConfig"`
}

// EBSConfigUpdateResponse is the request/reply response for either legacy
// configuration-update route.
type EBSConfigUpdateResponse struct {
	Volume  string `json:"Volume"`
	Success bool   `json:"Success"`
	Error   string `json:"Error"`
}
