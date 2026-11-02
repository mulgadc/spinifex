package podidentity

import (
	"bytes"
	"encoding/json"
)

// ClusterRoleName names the ClusterRole and ClusterRoleBinding the webhook runs as.
const ClusterRoleName = "spinifex:eks-pod-identity-webhook"

// RenderManifests returns the cluster objects the webhook needs, as JSON
// documents separated by "---" (valid YAML for the k3s manifest deployer): its
// least-privilege ClusterRole, the binding to its client-certificate user, and
// the MutatingWebhookConfiguration. The output is deterministic so every server
// writes the same bytes.
func RenderManifests(webhook WebhookConfig) ([]byte, error) {
	cfg, err := RenderWebhookConfiguration(webhook)
	if err != nil {
		return nil, err
	}
	role := map[string]any{
		"apiVersion": "rbac.authorization.k8s.io/v1",
		"kind":       "ClusterRole",
		"metadata":   map[string]any{"name": ClusterRoleName},
		"rules": []map[string]any{
			{"apiGroups": []string{""}, "resources": []string{"serviceaccounts"}, "verbs": []string{"get"}},
			{"apiGroups": []string{""}, "resources": []string{"namespaces"}, "verbs": []string{"list", "watch"}},
			{"apiGroups": []string{""}, "resources": []string{"configmaps"}, "verbs": []string{"create"}},
			{"apiGroups": []string{""}, "resources": []string{"configmaps"}, "resourceNames": []string{CAConfigMapName}, "verbs": []string{"get", "update"}},
		},
	}
	binding := map[string]any{
		"apiVersion": "rbac.authorization.k8s.io/v1",
		"kind":       "ClusterRoleBinding",
		"metadata":   map[string]any{"name": ClusterRoleName},
		"roleRef":    map[string]any{"apiGroup": "rbac.authorization.k8s.io", "kind": "ClusterRole", "name": ClusterRoleName},
		"subjects":   []map[string]any{{"apiGroup": "rbac.authorization.k8s.io", "kind": "User", "name": ClientCN}},
	}
	var out bytes.Buffer
	for i, doc := range []any{role, binding, json.RawMessage(cfg)} {
		b, err := json.MarshalIndent(doc, "", "  ")
		if err != nil {
			return nil, err
		}
		if i > 0 {
			out.WriteString("---\n")
		}
		out.Write(b)
		out.WriteByte('\n')
	}
	return out.Bytes(), nil
}
