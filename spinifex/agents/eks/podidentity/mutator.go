package podidentity

import (
	"cmp"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"strconv"
	"strings"
)

const (
	envRoleARN    = "AWS_ROLE_ARN"
	envTokenFile  = "AWS_WEB_IDENTITY_TOKEN_FILE" //nolint:gosec // variable name, not a credential
	envRegion     = "AWS_REGION"
	envDefRegion  = "AWS_DEFAULT_REGION"
	envRegionalS  = "AWS_STS_REGIONAL_ENDPOINTS"
	envSTSURL     = "AWS_ENDPOINT_URL_STS"
	envCABundle   = "AWS_CA_BUNDLE"
	envSSLCert    = "SSL_CERT_FILE"
	maxReviewSize = 8 << 20
)

type podObject struct {
	Metadata struct {
		Name         string            `json:"name"`
		GenerateName string            `json:"generateName"`
		Namespace    string            `json:"namespace"`
		Annotations  map[string]string `json:"annotations"`
	} `json:"metadata"`
	Spec struct {
		ServiceAccountName string            `json:"serviceAccountName"`
		ServiceAccount     string            `json:"serviceAccount"`
		Containers         []containerObject `json:"containers"`
		InitContainers     []containerObject `json:"initContainers"`
		Volumes            []nameOnly        `json:"volumes"`
	} `json:"spec"`
}

type nameOnly struct {
	Name string `json:"name"`
}

type containerObject struct {
	Name         string     `json:"name"`
	Env          []nameOnly `json:"env"`
	VolumeMounts []nameOnly `json:"volumeMounts"`
}

type envVar struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

type volumeMount struct {
	Name      string `json:"name"`
	MountPath string `json:"mountPath"`
	ReadOnly  bool   `json:"readOnly"`
}

// injection is what one pod gets, computed from its ServiceAccount and annotations.
type injection struct {
	roleARN  string
	regional bool
	skip     map[string]bool
}

// Mutate returns the patch for one pod creation request. It never denies a pod.
func (m *Mutator) Mutate(ctx context.Context, req AdmissionRequest) AdmissionResponse {
	resp := AdmissionResponse{UID: req.UID, Allowed: true}
	if req.Operation != "CREATE" {
		return resp
	}
	var pod podObject
	if err := json.Unmarshal(req.Object, &pod); err != nil {
		m.note(ctx, slog.LevelWarn, req.Namespace, "", ReasonDecode, "error", err)
		return resp
	}
	ns := cmp.Or(req.Namespace, pod.Metadata.Namespace)
	name := cmp.Or(pod.Metadata.Name, pod.Metadata.GenerateName)

	saName := cmp.Or(pod.Spec.ServiceAccountName, pod.Spec.ServiceAccount, "default")
	sa, err := m.sas.GetServiceAccount(ctx, ns, saName)
	if err != nil {
		if errors.Is(err, ErrServiceAccountNotFound) {
			m.note(ctx, slog.LevelInfo, ns, name, ReasonServiceAccountMissing, "serviceaccount", saName)
		} else {
			m.note(ctx, slog.LevelWarn, ns, name, ReasonServiceAccountLookup, "serviceaccount", saName, "error", err)
		}
		return resp
	}
	dom := cmp.Or(m.cfg.AnnotationDomain, DefaultAnnotationDomain)
	roleARN := sa.Annotations[dom+"/role-arn"]
	if roleARN == "" {
		return resp
	}

	in := injection{
		roleARN:  roleARN,
		regional: sa.Annotations[dom+"/sts-regional-endpoints"] == "true",
		skip:     map[string]bool{},
	}
	for c := range strings.SplitSeq(pod.Metadata.Annotations[dom+"/skip-containers"], ",") {
		if c = strings.TrimSpace(c); c != "" {
			in.skip[c] = true
		}
	}

	var ops []PatchOp
	patched := 0
	for _, set := range []struct {
		field string
		list  []containerObject
	}{{"containers", pod.Spec.Containers}, {"initContainers", pod.Spec.InitContainers}} {
		for i, c := range set.list {
			if in.skip[c.Name] {
				m.note(ctx, slog.LevelInfo, ns, name, ReasonSkipContainer, "container", c.Name)
				continue
			}
			cops := m.containerOps(fmt.Sprintf("/spec/%s/%d", set.field, i), c, in)
			if cops == nil {
				m.note(ctx, slog.LevelInfo, ns, name, ReasonAlreadyConfigured, "container", c.Name)
				continue
			}
			ops = append(ops, cops...)
			patched++
		}
	}
	if patched == 0 {
		return resp
	}

	ops = append(ops, volumeOps(pod, tokenExpiry(sa, pod.Metadata.Annotations, dom), cmp.Or(sa.Annotations[dom+"/audience"], DefaultAudience))...)
	resp.Patch = ops
	m.log.InfoContext(ctx, "pod identity injected", "namespace", ns, "pod", name, "serviceaccount", saName, "containers", patched)
	return resp
}

// note logs a skip or failure with every attribute inline, not on a derived logger.
func (m *Mutator) note(ctx context.Context, lvl slog.Level, ns, pod, reason string, kv ...any) {
	attrs := append([]any{"namespace", ns, "pod", pod, "reason", reason}, kv...)
	m.log.Log(ctx, lvl, "pod identity skipped", attrs...)
}

// containerOps patches one container, or returns nil when it already carries
// the role, region and regional STS settings.
func (m *Mutator) containerOps(path string, c containerObject, in injection) []PatchOp {
	has := map[string]bool{}
	for _, e := range c.Env {
		has[e.Name] = true
	}
	hasRole := has[envRoleARN] || has[envTokenFile]
	hasRegion := m.cfg.Region == "" || has[envRegion] || has[envDefRegion]
	hasSTS := has[envRegionalS]
	if hasRole && hasRegion && hasSTS {
		return nil
	}

	var vars []envVar
	add := func(name, value string) { vars = append(vars, envVar{name, value}) }
	if !hasRole {
		add(envRoleARN, in.roleARN)
		add(envTokenFile, TokenMountPath+"/"+TokenPath)
	}
	if !hasSTS && in.regional {
		add(envRegionalS, "regional")
	}
	if !hasRegion {
		add(envRegion, m.cfg.Region)
		add(envDefRegion, m.cfg.Region)
	}
	caPath := CAMountPath + "/" + CABundleKey
	for _, x := range []struct{ name, value string }{
		{envSTSURL, m.cfg.STSEndpoint}, {envCABundle, caPath}, {envSSLCert, caPath},
	} {
		if x.value != "" && !has[x.name] {
			add(x.name, x.value)
		}
	}

	mounted := map[string]bool{}
	for _, v := range c.VolumeMounts {
		mounted[v.Name] = true
	}
	var mounts []volumeMount
	if !mounted[TokenVolumeName] {
		mounts = append(mounts, volumeMount{TokenVolumeName, TokenMountPath, true})
	}
	if !mounted[CAVolumeName] {
		mounts = append(mounts, volumeMount{CAVolumeName, CAMountPath, true})
	}

	ops := appendOps(nil, path+"/env", len(c.Env) > 0, vars)
	ops = appendOps(ops, path+"/volumeMounts", len(c.VolumeMounts) > 0, mounts)
	if ops == nil {
		ops = []PatchOp{}
	}
	return ops
}

// appendOps adds items to an array, creating the array first when it is absent
// (an add to "/-" on a missing array is not a valid JSON patch).
func appendOps[T any](ops []PatchOp, path string, exists bool, items []T) []PatchOp {
	switch {
	case len(items) == 0:
		return ops
	case !exists:
		return append(ops, PatchOp{Op: "add", Path: path, Value: items})
	}
	for _, it := range items {
		ops = append(ops, PatchOp{Op: "add", Path: path + "/-", Value: it})
	}
	return ops
}

func volumeOps(pod podObject, expiry int64, audience string) []PatchOp {
	have := map[string]bool{}
	for _, v := range pod.Spec.Volumes {
		have[v.Name] = true
	}
	var vols []map[string]any
	if !have[TokenVolumeName] {
		vols = append(vols, map[string]any{
			"name": TokenVolumeName,
			"projected": map[string]any{"sources": []map[string]any{{
				"serviceAccountToken": map[string]any{"audience": audience, "expirationSeconds": expiry, "path": TokenPath},
			}}},
		})
	}
	if !have[CAVolumeName] {
		vols = append(vols, map[string]any{"name": CAVolumeName, "configMap": map[string]any{"name": CAConfigMapName}})
	}
	return appendOps(nil, "/spec/volumes", len(pod.Spec.Volumes) > 0, vols)
}

// tokenExpiry takes the pod annotation over the ServiceAccount's, clamps it to
// the allowed range and jitters the default so tokens do not all expire together.
func tokenExpiry(sa *ServiceAccount, podAnn map[string]string, dom string) int64 {
	key := dom + "/token-expiration"
	for _, raw := range []string{podAnn[key], sa.Annotations[key]} {
		if raw == "" {
			continue
		}
		if n, err := strconv.ParseInt(raw, 10, 64); err == nil {
			return min(max(n, MinTokenExpiration), MaxTokenExpiration)
		}
	}
	return DefaultMinTokenExpiration + rand.Int64N(MaxTokenExpiration-DefaultMinTokenExpiration+1) //nolint:gosec // jitter, not security
}

type admissionReview struct {
	APIVersion string             `json:"apiVersion"`
	Kind       string             `json:"kind"`
	Request    *AdmissionRequest  `json:"request,omitempty"`
	Response   *admissionResponse `json:"response,omitempty"`
}

type admissionResponse struct {
	UID       string `json:"uid"`
	Allowed   bool   `json:"allowed"`
	PatchType string `json:"patchType,omitempty"`
	Patch     string `json:"patch,omitempty"`
}

// ServeHTTP serves an AdmissionReview v1 request.
func (m *Mutator) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST required", http.StatusMethodNotAllowed)
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxReviewSize))
	if err != nil {
		http.Error(w, "read body", http.StatusBadRequest)
		return
	}
	var in admissionReview
	if err := json.Unmarshal(body, &in); err != nil || in.Request == nil {
		http.Error(w, "not an AdmissionReview", http.StatusBadRequest)
		return
	}

	res := m.Mutate(r.Context(), *in.Request)
	out := admissionReview{
		APIVersion: "admission.k8s.io/v1",
		Kind:       "AdmissionReview",
		Response:   &admissionResponse{UID: res.UID, Allowed: res.Allowed},
	}
	if len(res.Patch) > 0 {
		patch, err := json.Marshal(res.Patch)
		if err != nil {
			http.Error(w, "encode patch", http.StatusInternalServerError)
			return
		}
		out.Response.PatchType = "JSONPatch"
		out.Response.Patch = base64.StdEncoding.EncodeToString(patch)
	}
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(out); err != nil {
		m.log.Warn("pod identity response write failed", "error", err)
	}
}
