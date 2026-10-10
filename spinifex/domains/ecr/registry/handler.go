// Package ecrregistry implements ECR's OCI Distribution Spec v2 data-plane
// adapter (/v2/*). It owns the registry protocol mapping and OCI error
// envelope ({"errors":[{code,message,detail}]}), distinct from the AWS JSON
// 1.1 ECR control plane in package awsapi. Generic gateway code retains route
// assembly and authentication; this package owns no generic AWS transport.
package ecrregistry

import (
	"encoding/json"
	"log/slog"
	"net/http"
)

// OCIContentType is the OCI error/document content type registry clients expect.
const OCIContentType = "application/json"

// OCIError is one entry in an OCI Distribution error response.
type OCIError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	Detail  any    `json:"detail,omitempty"`
}

// ociErrorEnvelope is the OCI Distribution top-level error body.
type ociErrorEnvelope struct {
	Errors []OCIError `json:"errors"`
}

// WriteError writes an OCI Distribution error envelope with the given HTTP
// status. code is an OCI error code (e.g. "UNSUPPORTED", "MANIFEST_UNKNOWN").
func WriteError(w http.ResponseWriter, status int, code, message string) {
	body, err := json.Marshal(ociErrorEnvelope{Errors: []OCIError{{Code: code, Message: message}}})
	if err != nil {
		slog.Error("ECR/OCI: failed to marshal error envelope", "err", err)
		http.Error(w, `{"errors":[{"code":"UNKNOWN","message":"internal error"}]}`, http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", OCIContentType)
	w.WriteHeader(status)
	if _, err := w.Write(body); err != nil {
		slog.Error("ECR/OCI: failed to write error response", "err", err)
	}
}

// NotImplemented is the placeholder for every unimplemented /v2 endpoint: a
// 501 carrying the OCI "UNSUPPORTED" code so clients see a well-formed refusal.
func NotImplemented(w http.ResponseWriter, r *http.Request) {
	slog.Debug("ECR/OCI: endpoint not implemented", "method", r.Method, "path", r.URL.Path)
	WriteError(w, http.StatusNotImplemented, "UNSUPPORTED", "ECR registry endpoint not implemented")
}

// APIVersion handles GET /v2/ — the registry version-check probe. Real clients
// expect 200 with the Docker-Distribution-API-Version header even before any
// repository exists, so this endpoint is always live.
func APIVersion(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Docker-Distribution-Api-Version", "registry/2.0")
	w.WriteHeader(http.StatusOK)
}
