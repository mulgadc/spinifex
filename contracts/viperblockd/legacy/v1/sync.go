package viperblocklegacyv1

const (
	// SyncSubject is the legacy queue-group route that asks a viperblockd
	// worker to reload a mounted volume's state from its backend.
	SyncSubject = "ebs.sync"
)

// EBSSyncRequest identifies the volume whose mounted Viperblock state should
// be reloaded by the selected legacy worker.
type EBSSyncRequest struct {
	Volume string `json:"Volume"`
}

// EBSSyncResponse reports the selected worker's reload result. A worker that
// does not own the mounted volume returns an error rather than forwarding the
// request.
type EBSSyncResponse struct {
	Volume string `json:"Volume"`
	Synced bool   `json:"Synced"`
	Error  string `json:"Error"`
}
