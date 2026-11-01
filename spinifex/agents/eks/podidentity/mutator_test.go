package podidentity

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	testRoleARN  = "arn:aws:iam::000000000001:role/app"
	testRegion   = "ap-southeast-2"
	testSTS      = "https://sts.cluster.example:9999"
	annRole      = "eks.amazonaws.com/role-arn"
	annAudience  = "eks.amazonaws.com/audience"
	annExpiry    = "eks.amazonaws.com/token-expiration"
	annRegionalS = "eks.amazonaws.com/sts-regional-endpoints"
	annSkip      = "eks.amazonaws.com/skip-containers"
)

type fakeSAs struct {
	sas map[string]*ServiceAccount
	err error
}

func (f *fakeSAs) GetServiceAccount(_ context.Context, ns, name string) (*ServiceAccount, error) {
	if f.err != nil {
		return nil, f.err
	}
	if sa, ok := f.sas[ns+"/"+name]; ok {
		return sa, nil
	}
	return nil, ErrServiceAccountNotFound
}

// logRecords captures slog records for assertions.
type logRecords struct {
	mu   sync.Mutex
	recs []map[string]string
}

func (l *logRecords) Enabled(context.Context, slog.Level) bool { return true }
func (l *logRecords) WithAttrs([]slog.Attr) slog.Handler       { return l }
func (l *logRecords) WithGroup(string) slog.Handler            { return l }
func (l *logRecords) Handle(_ context.Context, r slog.Record) error {
	m := map[string]string{"msg": r.Message, "level": r.Level.String()}
	r.Attrs(func(a slog.Attr) bool {
		m[a.Key] = fmt.Sprint(a.Value.Any())
		return true
	})
	l.mu.Lock()
	l.recs = append(l.recs, m)
	l.mu.Unlock()
	return nil
}

// withReason returns the records at warn level or above that carry reason.
func (l *logRecords) withReason(reason string) []map[string]string {
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []map[string]string
	for _, r := range l.recs {
		if r["reason"] == reason {
			out = append(out, r)
		}
	}
	return out
}

func newMutator(sas ...*ServiceAccount) (*Mutator, *logRecords, *fakeSAs) {
	f := &fakeSAs{sas: map[string]*ServiceAccount{}}
	for i, sa := range sas {
		f.sas[fmt.Sprintf("app/sa%d", i)] = sa
	}
	rec := &logRecords{}
	return NewMutator(Config{Region: testRegion, STSEndpoint: testSTS}, f, slog.New(rec)), rec, f
}

func roleSA(extra map[string]string) *ServiceAccount {
	ann := map[string]string{annRole: testRoleARN}
	maps.Copy(ann, extra)
	return &ServiceAccount{Annotations: ann}
}

type env struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

func ctr(name string, envs ...env) map[string]any {
	c := map[string]any{"name": name, "image": "registry.example/app:1"}
	if len(envs) > 0 {
		var e []map[string]any
		for _, v := range envs {
			e = append(e, map[string]any{"name": v.Name, "value": v.Value})
		}
		c["env"] = e
	}
	return c
}

func podOf(sa string, ann map[string]string, containers ...map[string]any) map[string]any {
	spec := map[string]any{"containers": containers}
	if sa != "" {
		spec["serviceAccountName"] = sa
	}
	meta := map[string]any{"name": "web-1", "namespace": "app"}
	if ann != nil {
		meta["annotations"] = ann
	}
	return map[string]any{"apiVersion": "v1", "kind": "Pod", "metadata": meta, "spec": spec}
}

// The pod after the patch, decoded for assertions.
type outPod struct {
	Spec struct {
		Containers     []outCtr `json:"containers"`
		InitContainers []outCtr `json:"initContainers"`
		Volumes        []struct {
			Name      string `json:"name"`
			Projected *struct {
				Sources []struct {
					SAT *struct {
						Audience          string `json:"audience"`
						ExpirationSeconds int64  `json:"expirationSeconds"`
						Path              string `json:"path"`
					} `json:"serviceAccountToken"`
				} `json:"sources"`
			} `json:"projected"`
			ConfigMap *struct {
				Name string `json:"name"`
			} `json:"configMap"`
		} `json:"volumes"`
	} `json:"spec"`
}

type outCtr struct {
	Name string `json:"name"`
	Env  []env  `json:"env"`
	VM   []struct {
		Name      string `json:"name"`
		MountPath string `json:"mountPath"`
		ReadOnly  bool   `json:"readOnly"`
	} `json:"volumeMounts"`
}

func (c outCtr) envMap() map[string]string {
	m := map[string]string{}
	for _, e := range c.Env {
		m[e.Name] = e.Value
	}
	return m
}

func (c outCtr) mounts(name string) int {
	n := 0
	for _, v := range c.VM {
		if v.Name == name {
			n++
		}
	}
	return n
}

func (p outPod) volumes(name string) int {
	n := 0
	for _, v := range p.Spec.Volumes {
		if v.Name == name {
			n++
		}
	}
	return n
}

func (p outPod) tokenExpiry(t *testing.T) (aud string, exp int64) {
	t.Helper()
	for _, v := range p.Spec.Volumes {
		if v.Name == TokenVolumeName && v.Projected != nil && len(v.Projected.Sources) == 1 && v.Projected.Sources[0].SAT != nil {
			s := v.Projected.Sources[0].SAT
			assert.Equal(t, TokenPath, s.Path)
			return s.Audience, s.ExpirationSeconds
		}
	}
	require.Failf(t, "no projected token volume", "volumes: %+v", p.Spec.Volumes)
	return "", 0
}

// run mutates pod in namespace app and returns the patched pod and the response.
func run(t *testing.T, m *Mutator, pod map[string]any) (outPod, AdmissionResponse) {
	t.Helper()
	raw, err := json.Marshal(pod)
	require.NoError(t, err)
	resp := m.Mutate(context.Background(), AdmissionRequest{UID: "u1", Operation: "CREATE", Namespace: "app", Object: raw})
	require.True(t, resp.Allowed, "the mutator must never deny a pod")
	assert.Equal(t, "u1", resp.UID)
	patched := applyPatch(t, raw, resp.Patch)
	var out outPod
	require.NoError(t, json.Unmarshal(patched, &out))
	return out, resp
}

func TestMutate_InjectsTheAWSSetAndTheSpinifexExtras(t *testing.T) {
	m, _, _ := newMutator(roleSA(map[string]string{annRegionalS: "true"}))
	out, _ := run(t, m, podOf("sa0", nil, ctr("app")))

	aud, exp := out.tokenExpiry(t)
	assert.Equal(t, "sts.amazonaws.com", aud)
	assert.GreaterOrEqual(t, exp, DefaultMinTokenExpiration)
	assert.LessOrEqual(t, exp, MaxTokenExpiration)

	require.Len(t, out.Spec.Containers, 1)
	c := out.Spec.Containers[0]
	assert.Equal(t, map[string]string{
		"AWS_ROLE_ARN":                testRoleARN,
		"AWS_WEB_IDENTITY_TOKEN_FILE": TokenMountPath + "/" + TokenPath,
		"AWS_STS_REGIONAL_ENDPOINTS":  "regional",
		"AWS_REGION":                  testRegion,
		"AWS_DEFAULT_REGION":          testRegion,
		"AWS_ENDPOINT_URL_STS":        testSTS,
		"AWS_CA_BUNDLE":               CAMountPath + "/" + CABundleKey,
		"SSL_CERT_FILE":               CAMountPath + "/" + CABundleKey,
	}, c.envMap())

	assert.Equal(t, 1, c.mounts(TokenVolumeName))
	assert.Equal(t, 1, c.mounts(CAVolumeName))
	for _, v := range c.VM {
		assert.True(t, v.ReadOnly, "mount %s must be read-only", v.Name)
		switch v.Name {
		case TokenVolumeName:
			assert.Equal(t, TokenMountPath, v.MountPath)
		case CAVolumeName:
			assert.Equal(t, CAMountPath, v.MountPath)
		}
	}
	assert.Equal(t, 1, out.volumes(CAVolumeName))
	for _, v := range out.Spec.Volumes {
		if v.Name == CAVolumeName {
			require.NotNil(t, v.ConfigMap)
			assert.Equal(t, CAConfigMapName, v.ConfigMap.Name)
		}
	}
}

func TestMutate_RegionalSTSOnlyWhenTheServiceAccountAsksForIt(t *testing.T) {
	m, _, _ := newMutator(roleSA(nil), roleSA(map[string]string{annRegionalS: "false"}))
	for _, sa := range []string{"sa0", "sa1"} {
		out, _ := run(t, m, podOf(sa, nil, ctr("app")))
		require.Len(t, out.Spec.Containers, 1, sa)
		assert.NotContains(t, out.Spec.Containers[0].envMap(), "AWS_STS_REGIONAL_ENDPOINTS", sa)
		assert.Equal(t, testRoleARN, out.Spec.Containers[0].envMap()["AWS_ROLE_ARN"], sa)
	}
}

func TestMutate_KeepsACallersOwnTrustSettings(t *testing.T) {
	m, _, _ := newMutator(roleSA(nil))
	out, _ := run(t, m, podOf("sa0", nil,
		ctr("a", env{"SSL_CERT_FILE", "/etc/ssl/own.pem"}),
		ctr("b", env{"AWS_CA_BUNDLE", "/etc/ssl/own-aws.pem"}, env{"AWS_ENDPOINT_URL_STS", "https://own"})))

	a := out.Spec.Containers[0].envMap()
	assert.Equal(t, "/etc/ssl/own.pem", a["SSL_CERT_FILE"], "an existing SSL_CERT_FILE replaces the image trust store and must be left alone")
	assert.Equal(t, CAMountPath+"/"+CABundleKey, a["AWS_CA_BUNDLE"])
	b := out.Spec.Containers[1].envMap()
	assert.Equal(t, "/etc/ssl/own-aws.pem", b["AWS_CA_BUNDLE"])
	assert.Equal(t, "https://own", b["AWS_ENDPOINT_URL_STS"])
	assert.Equal(t, testRoleARN, b["AWS_ROLE_ARN"])
}

func TestMutate_InjectsInitContainersToo(t *testing.T) {
	m, _, _ := newMutator(roleSA(nil))
	pod := podOf("sa0", nil, ctr("app"))
	pod["spec"].(map[string]any)["initContainers"] = []map[string]any{ctr("setup")}
	out, _ := run(t, m, pod)

	require.Len(t, out.Spec.InitContainers, 1)
	assert.Equal(t, testRoleARN, out.Spec.InitContainers[0].envMap()["AWS_ROLE_ARN"])
	assert.Equal(t, 1, out.Spec.InitContainers[0].mounts(TokenVolumeName))
}

func TestMutate_LeavesAContainerWithAllThreeKeySetsUntouched(t *testing.T) {
	m, rec, _ := newMutator(roleSA(nil))
	complete := ctr("done",
		env{"AWS_ROLE_ARN", "arn:aws:iam::000000000001:role/own"},
		env{"AWS_WEB_IDENTITY_TOKEN_FILE", "/own/token"},
		env{"AWS_REGION", "us-east-1"},
		env{"AWS_STS_REGIONAL_ENDPOINTS", "legacy"})
	out, _ := run(t, m, podOf("sa0", nil, complete, ctr("plain")))

	got := out.Spec.Containers[0]
	assert.Len(t, got.Env, 4, "a complete container must not gain variables")
	assert.Zero(t, got.mounts(TokenVolumeName)+got.mounts(CAVolumeName))
	assert.Equal(t, testRoleARN, out.Spec.Containers[1].envMap()["AWS_ROLE_ARN"], "its neighbour is still injected")
	assert.NotEmpty(t, rec.withReason(ReasonAlreadyConfigured))
}

func TestMutate_AddsOnlyTheMissingKeysToAPartiallyConfiguredContainer(t *testing.T) {
	m, _, _ := newMutator(roleSA(map[string]string{annRegionalS: "true"}))
	partial := ctr("partial", env{"AWS_ROLE_ARN", "arn:aws:iam::000000000001:role/own"})
	out, _ := run(t, m, podOf("sa0", nil, partial))

	e := out.Spec.Containers[0].envMap()
	assert.Equal(t, "arn:aws:iam::000000000001:role/own", e["AWS_ROLE_ARN"], "the caller's role is kept")
	assert.NotContains(t, e, "AWS_WEB_IDENTITY_TOKEN_FILE", "role keys already defined are not completed")
	assert.Equal(t, testRegion, e["AWS_REGION"])
	assert.Equal(t, testRegion, e["AWS_DEFAULT_REGION"])
	assert.Equal(t, "regional", e["AWS_STS_REGIONAL_ENDPOINTS"])
	assert.Equal(t, 1, out.Spec.Containers[0].mounts(TokenVolumeName))
	count := 0
	for _, kv := range out.Spec.Containers[0].Env {
		if kv.Name == "AWS_ROLE_ARN" {
			count++
		}
	}
	assert.Equal(t, 1, count)
}

func TestMutate_DoesNotDuplicateAnExistingTokenVolume(t *testing.T) {
	m, _, _ := newMutator(roleSA(nil))
	pod := podOf("sa0", nil, ctr("app"))
	pod["spec"].(map[string]any)["volumes"] = []map[string]any{{
		"name": TokenVolumeName,
		"projected": map[string]any{"sources": []map[string]any{{
			"serviceAccountToken": map[string]any{"audience": "own", "expirationSeconds": 7200, "path": "token"},
		}}},
	}}
	out, _ := run(t, m, pod)

	assert.Equal(t, 1, out.volumes(TokenVolumeName))
	aud, exp := out.tokenExpiry(t)
	assert.Equal(t, "own", aud, "the caller's volume is kept")
	assert.Equal(t, int64(7200), exp)
	assert.Equal(t, 1, out.Spec.Containers[0].mounts(TokenVolumeName))
}

func TestMutate_AudienceAndExpiry(t *testing.T) {
	cases := []struct {
		name    string
		saAnn   map[string]string
		podAnn  map[string]string
		wantAud string
		wantExp int64
	}{
		{"service account audience", map[string]string{annAudience: "custom.example"}, nil, "custom.example", 0},
		{"service account expiry", map[string]string{annExpiry: "7200"}, nil, "sts.amazonaws.com", 7200},
		{"pod expiry wins over service account", map[string]string{annExpiry: "7200"}, map[string]string{annExpiry: "1200"}, "sts.amazonaws.com", 1200},
		{"below the minimum is raised to 600", nil, map[string]string{annExpiry: "100"}, "sts.amazonaws.com", 600},
		{"above the maximum is lowered to 86400", map[string]string{annExpiry: "999999"}, nil, "sts.amazonaws.com", 86400},
		{"invalid expiry falls back to the default", map[string]string{annExpiry: "soon"}, nil, "sts.amazonaws.com", 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m, _, _ := newMutator(roleSA(tc.saAnn))
			out, _ := run(t, m, podOf("sa0", tc.podAnn, ctr("app")))
			aud, exp := out.tokenExpiry(t)
			assert.Equal(t, tc.wantAud, aud)
			if tc.wantExp != 0 {
				assert.Equal(t, tc.wantExp, exp)
				return
			}
			assert.GreaterOrEqual(t, exp, MaxTokenExpiration-7200)
			assert.LessOrEqual(t, exp, MaxTokenExpiration)
		})
	}
}

func TestMutate_DefaultExpiryIsJitteredWithinTwoHours(t *testing.T) {
	m, _, _ := newMutator(roleSA(nil))
	seen := map[int64]bool{}
	for range 60 {
		out, _ := run(t, m, podOf("sa0", nil, ctr("app")))
		_, exp := out.tokenExpiry(t)
		require.GreaterOrEqual(t, exp, DefaultMinTokenExpiration)
		require.LessOrEqual(t, exp, MaxTokenExpiration)
		seen[exp] = true
	}
	assert.Greater(t, len(seen), 1, "60 pods all got the same default expiry; it is not jittered")
}

func TestMutate_SkipContainersAnnotation(t *testing.T) {
	m, rec, _ := newMutator(roleSA(nil))
	out, _ := run(t, m, podOf("sa0", map[string]string{annSkip: "sidecar, mesh"},
		ctr("app"), ctr("sidecar"), ctr("mesh")))

	assert.Equal(t, testRoleARN, out.Spec.Containers[0].envMap()["AWS_ROLE_ARN"])
	for _, i := range []int{1, 2} {
		c := out.Spec.Containers[i]
		assert.Empty(t, c.Env, c.Name)
		assert.Zero(t, c.mounts(TokenVolumeName), c.Name)
	}
	assert.Len(t, rec.withReason(ReasonSkipContainer), 2)
}

func TestMutate_LeavesPodsOfUnannotatedServiceAccountsAlone(t *testing.T) {
	m, _, f := newMutator(&ServiceAccount{Annotations: map[string]string{"other": "x"}}, &ServiceAccount{})
	f.sas["app/default"] = &ServiceAccount{}
	for _, sa := range []string{"sa0", "sa1", ""} {
		_, resp := run(t, m, podOf(sa, nil, ctr("app")))
		assert.Empty(t, resp.Patch, "service account %q", sa)
	}
}

func TestMutate_UsesTheDefaultServiceAccountWhenThePodNamesNone(t *testing.T) {
	m, _, f := newMutator()
	f.sas["app/default"] = roleSA(nil)
	out, _ := run(t, m, podOf("", nil, ctr("app")))
	assert.Equal(t, testRoleARN, out.Spec.Containers[0].envMap()["AWS_ROLE_ARN"])
}

// A pod of a managed add-on as render_addon writes it: the web identity,
// region and regional sts settings, the token volume and the gateway CA.
func TestMutate_LeavesARenderedManagedAddonPodUnchanged(t *testing.T) {
	m, _, _ := newMutator(roleSA(map[string]string{annRegionalS: "true"}))
	c := ctr("controller",
		env{"AWS_ROLE_ARN", testRoleARN},
		env{"AWS_WEB_IDENTITY_TOKEN_FILE", TokenMountPath + "/token"},
		env{"AWS_STS_REGIONAL_ENDPOINTS", "regional"},
		env{"AWS_REGION", testRegion},
		env{"AWS_DEFAULT_REGION", testRegion},
		env{"AWS_ENDPOINT_URL_STS", testSTS},
		env{"AWS_CA_BUNDLE", "/etc/spinifex/gateway-ca/ca.pem"},
		env{"SSL_CERT_FILE", "/etc/spinifex/gateway-ca/ca.pem"})
	c["volumeMounts"] = []map[string]any{
		{"name": TokenVolumeName, "mountPath": TokenMountPath, "readOnly": true},
		{"name": "spinifex-gateway-ca", "mountPath": "/etc/spinifex/gateway-ca", "readOnly": true},
	}
	pod := podOf("sa0", nil, c)
	pod["spec"].(map[string]any)["volumes"] = []map[string]any{
		{"name": TokenVolumeName, "projected": map[string]any{"sources": []map[string]any{{
			"serviceAccountToken": map[string]any{"audience": "sts.amazonaws.com", "expirationSeconds": 86400, "path": "token"}}}}},
		{"name": "spinifex-gateway-ca", "secret": map[string]any{"secretName": "spinifex-gateway-ca"}},
	}
	_, resp := run(t, m, pod)
	assert.Empty(t, resp.Patch, "a managed add-on pod must come out byte for byte as it went in")
}

func TestMutate_OnlyActsOnPodCreation(t *testing.T) {
	m, _, _ := newMutator(roleSA(nil))
	raw, err := json.Marshal(podOf("sa0", nil, ctr("app")))
	require.NoError(t, err)
	resp := m.Mutate(context.Background(), AdmissionRequest{UID: "u", Operation: "UPDATE", Namespace: "app", Object: raw})
	assert.True(t, resp.Allowed)
	assert.Empty(t, resp.Patch)
}

func TestMutate_LogsEverySkipAndFailureWithNamespacePodAndReason(t *testing.T) {
	t.Run("lookup failure fails open", func(t *testing.T) {
		m, rec, f := newMutator(roleSA(nil))
		f.err = errors.New("apiserver unavailable")
		_, resp := run(t, m, podOf("sa0", nil, ctr("app")))
		assert.Empty(t, resp.Patch)
		got := rec.withReason(ReasonServiceAccountLookup)
		require.Len(t, got, 1)
		assert.Equal(t, "app", got[0]["namespace"])
		assert.Equal(t, "web-1", got[0]["pod"])
		assert.Contains(t, got[0]["error"], "apiserver unavailable")
	})
	t.Run("missing service account", func(t *testing.T) {
		m, rec, _ := newMutator()
		_, resp := run(t, m, podOf("ghost", nil, ctr("app")))
		assert.Empty(t, resp.Patch)
		require.Len(t, rec.withReason(ReasonServiceAccountMissing), 1)
	})
	t.Run("undecodable object", func(t *testing.T) {
		m, rec, _ := newMutator()
		resp := m.Mutate(context.Background(), AdmissionRequest{UID: "u", Operation: "CREATE", Namespace: "app", Object: json.RawMessage(`{"spec":`)})
		assert.True(t, resp.Allowed)
		assert.Empty(t, resp.Patch)
		got := rec.withReason(ReasonDecode)
		require.Len(t, got, 1)
		assert.Equal(t, "app", got[0]["namespace"])
	})
	t.Run("a pod with only generateName is named by it", func(t *testing.T) {
		m, rec, _ := newMutator(roleSA(nil))
		pod := podOf("sa0", map[string]string{annSkip: "app"}, ctr("app"))
		meta := pod["metadata"].(map[string]any)
		delete(meta, "name")
		meta["generateName"] = "web-"
		run(t, m, pod)
		got := rec.withReason(ReasonSkipContainer)
		require.Len(t, got, 1)
		assert.True(t, strings.HasPrefix(got[0]["pod"], "web-"), "pod attr %q", got[0]["pod"])
	})
	t.Run("an unannotated pod is not a skip worth logging", func(t *testing.T) {
		m, rec, _ := newMutator(&ServiceAccount{})
		run(t, m, podOf("sa0", nil, ctr("app")))
		for _, r := range rec.recs {
			assert.NotEqual(t, "WARN", r["level"], "%v", r)
			assert.NotEqual(t, "ERROR", r["level"], "%v", r)
		}
	})
}

func TestServeHTTP_AnswersAnAdmissionReview(t *testing.T) {
	m, _, _ := newMutator(roleSA(nil))
	obj, err := json.Marshal(podOf("sa0", nil, ctr("app")))
	require.NoError(t, err)
	body, err := json.Marshal(map[string]any{
		"apiVersion": "admission.k8s.io/v1", "kind": "AdmissionReview",
		"request": map[string]any{"uid": "abc-123", "operation": "CREATE", "namespace": "app", "object": json.RawMessage(obj)},
	})
	require.NoError(t, err)

	rr := httptest.NewRecorder()
	m.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/mutate", bytes.NewReader(body)))
	require.Equal(t, http.StatusOK, rr.Code)

	var review struct {
		APIVersion string `json:"apiVersion"`
		Kind       string `json:"kind"`
		Response   struct {
			UID       string `json:"uid"`
			Allowed   bool   `json:"allowed"`
			PatchType string `json:"patchType"`
			Patch     string `json:"patch"`
		} `json:"response"`
	}
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &review))
	assert.Equal(t, "admission.k8s.io/v1", review.APIVersion)
	assert.Equal(t, "AdmissionReview", review.Kind)
	assert.Equal(t, "abc-123", review.Response.UID)
	assert.True(t, review.Response.Allowed)
	assert.Equal(t, "JSONPatch", review.Response.PatchType)
	patch, err := base64.StdEncoding.DecodeString(review.Response.Patch)
	require.NoError(t, err)
	assert.Contains(t, string(patch), "AWS_ROLE_ARN")
}

// applyPatch applies add, replace and remove operations to raw.
func applyPatch(t *testing.T, raw []byte, ops []PatchOp) []byte {
	t.Helper()
	var doc any
	require.NoError(t, json.Unmarshal(raw, &doc))
	for _, op := range ops {
		parts := strings.Split(strings.TrimPrefix(op.Path, "/"), "/")
		for i := range parts {
			parts[i] = strings.ReplaceAll(strings.ReplaceAll(parts[i], "~1", "/"), "~0", "~")
		}
		var val any
		if op.Value != nil {
			b, err := json.Marshal(op.Value)
			require.NoError(t, err)
			require.NoError(t, json.Unmarshal(b, &val))
		}
		doc = patchAt(t, doc, parts, op.Op, val)
	}
	out, err := json.Marshal(doc)
	require.NoError(t, err)
	return out
}

func patchAt(t *testing.T, node any, parts []string, op string, val any) any {
	t.Helper()
	if len(parts) == 1 {
		key := parts[0]
		switch n := node.(type) {
		case map[string]any:
			switch op {
			case "add", "replace":
				n[key] = val
			case "remove":
				delete(n, key)
			default:
				require.Failf(t, "unsupported op", "%s", op)
			}
			return n
		case []any:
			if key == "-" {
				return append(n, val)
			}
			i, err := strconv.Atoi(key)
			require.NoError(t, err)
			switch op {
			case "add":
				n = append(n, nil)
				copy(n[i+1:], n[i:])
				n[i] = val
			case "replace":
				n[i] = val
			case "remove":
				n = append(n[:i], n[i+1:]...)
			}
			return n
		}
		require.Fail(t, "patch parent is neither object nor array")
	}
	switch n := node.(type) {
	case map[string]any:
		child, ok := n[parts[0]]
		require.True(t, ok, "patch path parent %q missing", parts[0])
		n[parts[0]] = patchAt(t, child, parts[1:], op, val)
		return n
	case []any:
		i, err := strconv.Atoi(parts[0])
		require.NoError(t, err)
		n[i] = patchAt(t, n[i], parts[1:], op, val)
		return n
	}
	require.Fail(t, "patch path crosses a scalar")
	return nil
}
