package oci_test

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"io"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"strings"
	"testing"

	"github.com/oracle/oci-go-sdk/v65/common"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mulgadc/spinifex/spinifex/cloud/oci"
)

// capture records what the SDK actually put on the wire, which is the point of
// these tests: the request shape is the part of client.go that can be wrong in
// a way no amount of reading catches.
type capture struct {
	method string
	path   string
	query  url.Values
	body   map[string]any
}

// testClient wires the real SDK-backed client to a handler. The signing key is
// generated per test — the SDK refuses to build a client without one, and a
// throwaway key is enough because nothing verifies the signature here.
func testClient(t *testing.T, h http.HandlerFunc) (oci.Client, *[]capture) {
	t.Helper()
	var seen []capture
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c := capture{method: r.Method, path: r.URL.Path, query: r.URL.Query()}
		if b, _ := io.ReadAll(r.Body); len(b) > 0 {
			_ = json.Unmarshal(b, &c.body)
		}
		seen = append(seen, c)
		w.Header().Set("Content-Type", "application/json")
		h(w, r)
	}))
	t.Cleanup(srv.Close)

	key, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)
	pemKey := pem.EncodeToMemory(&pem.Block{
		Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key),
	})
	provider := common.NewRawConfigurationProvider(
		"ocid1.tenancy.oc1..t", "ocid1.user.oc1..u", "ap-sydney-1",
		"aa:bb:cc:dd:ee:ff:00:11:22:33:44:55:66:77:88:99", string(pemKey), nil)

	c, err := oci.NewTestClient(provider, strings.TrimPrefix(srv.URL, "https://"), srv.Client())
	require.NoError(t, err)
	return c, &seen
}

// OCI pages list responses and a VNIC can hold 64 addresses. A client that
// ignored opc-next-page would report a partial VNIC to reconcile, which then
// treats the addresses it did not see as absent — and drops live bindings.
func TestListPrivateIPsFollowsEveryPage(t *testing.T) {
	c, seen := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Query().Get("page") {
		case "":
			w.Header().Set("opc-next-page", "page2")
			_, _ = w.Write([]byte(`[{"id":"p1","ipAddress":"10.200.0.100","vnicId":"v1"}]`))
		case "page2":
			w.Header().Set("opc-next-page", "page3")
			_, _ = w.Write([]byte(`[{"id":"p2","ipAddress":"10.200.0.101","vnicId":"v1"}]`))
		default:
			_, _ = w.Write([]byte(`[{"id":"p3","ipAddress":"10.200.0.102","vnicId":"v1"}]`))
		}
	})

	got, err := c.ListPrivateIPs(context.Background(), "v1")
	require.NoError(t, err)
	require.Len(t, got, 3, "pagination stopped early — reconcile would treat the rest as absent")
	assert.Equal(t, "10.200.0.102", got[2].Address.String())
	assert.Len(t, *seen, 3)
	assert.Equal(t, "v1", (*seen)[0].query.Get("vnicId"))
}

func TestListPrivateIPsStopsOnASinglePage(t *testing.T) {
	c, seen := testClient(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`[{"id":"p1","ipAddress":"10.200.0.100","vnicId":"v1"}]`))
	})

	got, err := c.ListPrivateIPs(context.Background(), "v1")
	require.NoError(t, err)
	assert.Len(t, got, 1)
	assert.Len(t, *seen, 1, "a response with no next page must not be re-fetched")
}

// The bug this guards against is silent: a nil *string is omitted from the JSON
// body, so OCI would read the update as "change nothing", return 200, and leave
// the public IP attached. The address would keep billing and keep pointing at a
// released instance.
func TestDetachPublicIPSendsAnExplicitEmptyPrivateIP(t *testing.T) {
	c, seen := testClient(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"id":"q1","ipAddress":"203.0.113.10","lifecycleState":"UNASSIGNED"}`))
	})

	got, err := c.DetachPublicIP(context.Background(), "q1")
	require.NoError(t, err)
	assert.False(t, got.IsAssigned())

	require.Len(t, *seen, 1)
	body := (*seen)[0].body
	raw, present := body["privateIpId"]
	require.True(t, present, "privateIpId was omitted, so OCI reads the update as a no-op and the address stays attached")
	assert.Empty(t, raw)
}

func TestAttachPublicIPNamesTheTargetPrivateIP(t *testing.T) {
	c, seen := testClient(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"id":"q1","privateIpId":"p2","lifecycleState":"ASSIGNED"}`))
	})

	got, err := c.AttachPublicIP(context.Background(), "q1", "p2")
	require.NoError(t, err)
	assert.True(t, got.IsAssigned())
	assert.Equal(t, "p2", (*seen)[0].body["privateIpId"])
}

// An EIP has to outlive its association, so the create must ask for RESERVED.
// An EPHEMERAL public IP would vanish when the instance released it, which is
// not what AWS AllocateAddress promises.
func TestCreatePublicIPAsksForAReservedLifetime(t *testing.T) {
	c, seen := testClient(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"id":"q1","ipAddress":"203.0.113.10","lifetime":"RESERVED","lifecycleState":"ASSIGNING"}`))
	})

	got, err := c.CreatePublicIP(context.Background(), "comp1", "p1", "spinifex-eipalloc-1")
	require.NoError(t, err)
	assert.Equal(t, oci.LifetimeReserved, got.Lifetime)
	assert.False(t, got.IsAssigned(), "a create returns ASSIGNING; the caller must poll")

	body := (*seen)[0].body
	assert.Equal(t, "RESERVED", body["lifetime"])
	assert.Equal(t, "comp1", body["compartmentId"])
	assert.Equal(t, "p1", body["privateIpId"])
}

// OCI picks the address unless we name one, and the allocator relies on that:
// naming one would mean guessing an address OCI may already have handed out.
func TestAssignPrivateIPOmitsTheAddressWhenWeDoNotChooseIt(t *testing.T) {
	c, seen := testClient(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"id":"p1","ipAddress":"10.200.0.183","vnicId":"v1"}`))
	})

	got, err := c.AssignPrivateIP(context.Background(), "v1", netip.Addr{}, "spinifex-eipalloc-1")
	require.NoError(t, err)
	assert.Equal(t, "10.200.0.183", got.Address.String())

	body := (*seen)[0].body
	assert.Equal(t, "v1", body["vnicId"])
	assert.Equal(t, "spinifex-eipalloc-1", body["displayName"])
	_, present := body["ipAddress"]
	assert.False(t, present, "an omitted address is how OCI is asked to choose")
}

// A 404 from the real transport has to reach the caller as ErrNotFound, or
// reconcile cannot tell "already gone" from "the API is broken" and would stop
// mid-pass rather than finishing the cleanup.
func TestServiceErrorsFromTheWireClassify(t *testing.T) {
	c, _ := testClient(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"code":"NotAuthorizedOrNotFound","message":"Authorization failed or requested resource not found."}`))
	})

	_, err := c.GetPublicIP(context.Background(), "q1")
	require.Error(t, err)
	assert.ErrorIs(t, err, oci.ErrNotFound)
}

func TestQuotaErrorsFromTheWireClassify(t *testing.T) {
	c, _ := testClient(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"code":"LimitExceeded","message":"public IP limit reached"}`))
	})

	_, err := c.CreatePublicIP(context.Background(), "comp1", "p1", "spinifex-1")
	require.Error(t, err)
	assert.ErrorIs(t, err, oci.ErrLimitExceeded)
}

func TestDeleteAndUnassignReachTheRightRoutes(t *testing.T) {
	c, seen := testClient(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})

	require.NoError(t, c.DeletePublicIP(context.Background(), "q1"))
	require.NoError(t, c.UnassignPrivateIP(context.Background(), "p1"))

	require.Len(t, *seen, 2)
	assert.Equal(t, http.MethodDelete, (*seen)[0].method)
	assert.Contains(t, (*seen)[0].path, "q1")
	assert.Equal(t, http.MethodDelete, (*seen)[1].method)
	assert.Contains(t, (*seen)[1].path, "p1")
}

func TestGetPublicIPByPrivateIPIDAsksByPrivateIP(t *testing.T) {
	c, seen := testClient(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"id":"q1","privateIpId":"p1","lifecycleState":"ASSIGNED"}`))
	})

	got, err := c.GetPublicIPByPrivateIPID(context.Background(), "p1")
	require.NoError(t, err)
	assert.Equal(t, "q1", got.ID)
	assert.Equal(t, "p1", (*seen)[0].body["privateIpId"])
}
