package eksv1

// StagedAddonManifest describes one add-on staged for delivery: the bundled
// add-on and version plus the operator config the guest renders it with. The
// json tags are the host-internal NATS form; see InternalAddonsResponse.
type StagedAddonManifest struct {
	AddonName             string `json:"addonName"`
	AddonVersion          string `json:"addonVersion"`
	ServiceAccountRoleArn string `json:"serviceAccountRoleArn,omitempty"`
	ConfigurationValues   string `json:"configurationValues,omitempty"`
}

// InternalAddonsResponse is the internal-addons HTTP body. The gateway's SDK
// REST-JSON marshaller ignores json tags, so Go field names (and order) are the
// deployed bytes; the guest decodes them case-insensitively with encoding/json.
type InternalAddonsResponse struct {
	Addons []StagedAddonManifest `json:"addons"`
}
