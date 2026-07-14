package subscribers

import (
	"encoding/json"
	"log/slog"

	"github.com/nats-io/nats.go"
)

type respondResponse struct {
	Success bool   `json:"success"`
	Error   string `json:"error,omitempty"`
}

// respond sends a JSON success/error envelope to a NATS request. Fire-and-
// forget when no reply is set on msg.
func respond(msg *nats.Msg, err error) {
	if msg.Reply == "" {
		return
	}
	resp := respondResponse{Success: true}
	if err != nil {
		resp.Success = false
		resp.Error = err.Error()
	}
	data, marshalErr := json.Marshal(resp)
	if marshalErr != nil {
		slog.Error("subscribers: failed to marshal NATS response", "err", marshalErr)
		return
	}
	if err := msg.Respond(data); err != nil {
		slog.Error("subscribers: failed to respond to NATS request", "err", err)
	}
}
