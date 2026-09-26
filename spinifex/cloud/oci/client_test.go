package oci_test

import (
	"errors"
	"net/netip"
	"testing"

	ocisdk "github.com/oracle/oci-go-sdk/v65/common"
	"github.com/oracle/oci-go-sdk/v65/core"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mulgadc/spinifex/spinifex/cloud/oci"
)

// serviceError is an SDK service error. common.IsServiceError matches on the
// interface, so a local type is enough and no HTTP round trip is needed.
type serviceError struct {
	code    string
	message string
	status  int
	reqID   string
}

func (e serviceError) Error() string           { return e.message }
func (e serviceError) GetHTTPStatusCode() int  { return e.status }
func (e serviceError) GetMessage() string      { return e.message }
func (e serviceError) GetCode() string         { return e.code }
func (e serviceError) GetOpcRequestID() string { return e.reqID }

var _ ocisdk.ServiceError = serviceError{}

// The classification is control flow, not cosmetics: a quota refusal has to
// reach the customer as InsufficientAddressCapacity, a 404 has to let reconcile
// treat an object as already gone, and a conflict has to trigger a re-read
// rather than a blind retry.
func TestWrapClassifiesTheStatusesThatChangeBehaviour(t *testing.T) {
	for _, tc := range []struct {
		name   string
		err    serviceError
		want   error
		reason string
	}{
		{"404 is not found", serviceError{status: 404, code: "NotAuthorizedOrNotFound"}, oci.ErrNotFound,
			"OCI returns 404 for both missing and unauthorised, and either way the object is not ours to use"},
		{"409 is a conflict", serviceError{status: 409, code: "Conflict"}, oci.ErrConflict, ""},
		{"412 is a conflict", serviceError{status: 412, code: "PreconditionFailed"}, oci.ErrConflict,
			"an etag mismatch means another node changed it first"},
		{"400 LimitExceeded is a quota", serviceError{status: 400, code: "LimitExceeded"}, oci.ErrLimitExceeded,
			"64 private IPs per VNIC and 50 public IPs per region arrive as this"},
		{"429 is a quota", serviceError{status: 429, code: "TooManyRequests"}, oci.ErrLimitExceeded, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := oci.Wrap("TestOp", tc.err)
			require.Error(t, err)
			assert.ErrorIs(t, err, tc.want, tc.reason)
		})
	}
}

// A 400 that is not a quota must not read as one, or a malformed request would
// be reported to the customer as the region being full.
func TestWrapLeavesAnUnclassifiedStatusUnmatched(t *testing.T) {
	err := oci.Wrap("TestOp", serviceError{status: 400, code: "InvalidParameter"})
	require.Error(t, err)
	assert.NotErrorIs(t, err, oci.ErrLimitExceeded)
	assert.NotErrorIs(t, err, oci.ErrNotFound)
	assert.NotErrorIs(t, err, oci.ErrConflict)
}

func TestWrapPassesANonServiceErrorThrough(t *testing.T) {
	sentinel := errors.New("dial tcp: connection refused")
	err := oci.Wrap("TestOp", sentinel)
	require.Error(t, err)
	assert.ErrorIs(t, err, sentinel, "a transport failure must stay unwrappable to its cause")
	assert.Contains(t, err.Error(), "TestOp")
}

func TestWrapOfNilIsNil(t *testing.T) {
	assert.NoError(t, oci.Wrap("TestOp", nil))
}

// The request ID is what Oracle support asks for first, so it has to survive
// into the message rather than only into a struct field nobody prints.
func TestAPIErrorMessageCarriesTheRequestID(t *testing.T) {
	err := oci.Wrap("CreatePublicIp", serviceError{
		status: 400, code: "LimitExceeded", message: "public IP limit reached", reqID: "ABC123",
	})
	assert.Contains(t, err.Error(), "ABC123")
	assert.Contains(t, err.Error(), "public IP limit reached")
	assert.Contains(t, err.Error(), "CreatePublicIp")
	assert.Contains(t, err.Error(), "400")
}

// Every field on an SDK response is a pointer, and OCI omits the ones it has
// no value for. A converter that dereferenced blind would panic on exactly the
// responses that matter most — an ASSIGNING public IP has no address yet.
func TestConvertersToleratePointerFieldsOCIOmits(t *testing.T) {
	priv, err := oci.ToPrivateIP(core.PrivateIp{})
	require.NoError(t, err)
	assert.Empty(t, priv.ID)
	assert.False(t, priv.Address.IsValid(), "an absent address must be the zero Addr, not a parse error")
	assert.False(t, priv.IsPrimary)

	pub, err := oci.ToPublicIP(core.PublicIp{})
	require.NoError(t, err)
	assert.False(t, pub.Address.IsValid())
	assert.False(t, pub.IsAssigned())
}

func TestConvertersCarryTheFieldsTheAllocatorReadsBack(t *testing.T) {
	priv, err := oci.ToPrivateIP(core.PrivateIp{
		Id:          strptr("ocid1.privateip.oc1..p1"),
		IpAddress:   strptr("10.200.0.183"),
		VnicId:      strptr("ocid1.vnic.oc1..v1"),
		SubnetId:    strptr("ocid1.subnet.oc1..s1"),
		DisplayName: strptr("spinifex-eipalloc-1"),
		IsPrimary:   boolptr(true),
	})
	require.NoError(t, err)
	assert.Equal(t, netip.MustParseAddr("10.200.0.183"), priv.Address)
	assert.Equal(t, "ocid1.vnic.oc1..v1", priv.VNICID)
	assert.True(t, priv.IsPrimary)

	pub, err := oci.ToPublicIP(core.PublicIp{
		Id:             strptr("ocid1.publicip.oc1..q1"),
		IpAddress:      strptr("161.33.227.97"),
		PrivateIpId:    strptr("ocid1.privateip.oc1..p1"),
		Lifetime:       core.PublicIpLifetimeReserved,
		LifecycleState: core.PublicIpLifecycleStateAssigned,
	})
	require.NoError(t, err)
	assert.Equal(t, netip.MustParseAddr("161.33.227.97"), pub.Address)
	assert.Equal(t, oci.LifetimeReserved, pub.Lifetime)
	assert.True(t, pub.IsAssigned(), "an ASSIGNED public IP is the only one safe to hand out")
}

// A malformed address has to be an error, not a silently zero Addr that the
// allocator would then record as a customer's public IP.
func TestConvertersRefuseAnUnparseableAddress(t *testing.T) {
	_, err := oci.ToPrivateIP(core.PrivateIp{IpAddress: strptr("not-an-ip")})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not-an-ip")

	_, err = oci.ToPublicIP(core.PublicIp{IpAddress: strptr("999.1.1.1")})
	require.Error(t, err)
}

func TestDerefOfNilIsEmpty(t *testing.T) {
	assert.Empty(t, oci.Deref(nil))
	assert.Equal(t, "x", oci.Deref(strptr("x")))
}

func strptr(s string) *string { return &s }
func boolptr(b bool) *bool    { return &b }
