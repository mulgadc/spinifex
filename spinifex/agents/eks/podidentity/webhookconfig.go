package podidentity

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"net/url"
)

// WebhookName is the fully qualified name of the webhook entry.
const WebhookName = "pod-identity.spinifex.mulgadc.com"

// RenderWebhookConfiguration returns the MutatingWebhookConfiguration as JSON.
// The URL must be https on 127.0.0.1: each API server calls its own webhook, and
// failurePolicy Ignore keeps an unreachable injector from blocking pod creation.
func RenderWebhookConfiguration(c WebhookConfig) ([]byte, error) {
	if c.Name == "" {
		return nil, errors.New("webhook configuration needs a name")
	}
	u, err := url.Parse(c.URL)
	if err != nil || u.Scheme != "https" || u.Hostname() != "127.0.0.1" {
		return nil, fmt.Errorf("webhook url %q must be https on 127.0.0.1", c.URL)
	}
	if len(c.CA) == 0 {
		return nil, errors.New("webhook configuration needs a CA bundle")
	}
	cfg := map[string]any{
		"apiVersion": "admissionregistration.k8s.io/v1",
		"kind":       "MutatingWebhookConfiguration",
		"metadata":   map[string]any{"name": c.Name},
		"webhooks": []map[string]any{{
			"name": WebhookName,
			"clientConfig": map[string]any{
				"url":      c.URL,
				"caBundle": base64.StdEncoding.EncodeToString(c.CA),
			},
			"rules": []map[string]any{{
				"operations":  []string{"CREATE"},
				"apiGroups":   []string{""},
				"apiVersions": []string{"v1"},
				"resources":   []string{"pods"},
				"scope":       "Namespaced",
			}},
			"failurePolicy":           "Ignore",
			"sideEffects":             "None",
			"timeoutSeconds":          5,
			"admissionReviewVersions": []string{"v1"},
		}},
	}
	return json.Marshal(cfg)
}

// BuildCABundle returns the system roots plus the gateway CA as one PEM bundle,
// without duplicate certificates.
func BuildCABundle(system, gateway []byte) []byte {
	seen := map[[sha256.Size]byte]bool{}
	var out []byte
	for _, src := range [][]byte{system, gateway} {
		for rest := src; ; {
			var b *pem.Block
			b, rest = pem.Decode(rest)
			if b == nil {
				break
			}
			if b.Type != "CERTIFICATE" {
				continue
			}
			sum := sha256.Sum256(b.Bytes)
			if seen[sum] {
				continue
			}
			seen[sum] = true
			out = append(out, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: b.Bytes})...)
		}
	}
	return out
}
